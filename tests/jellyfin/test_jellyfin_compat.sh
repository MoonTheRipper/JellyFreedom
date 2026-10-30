#!/usr/bin/env bash
# test_jellyfin_compat.sh — run JellyFreedom's Jellyfin client against real, throwaway
# Jellyfin servers, one per version, and measure what each server does with a library of
# .strm files.
#
#   tests/jellyfin/test_jellyfin_compat.sh             10.11.11 and 12.4
#   tests/jellyfin/test_jellyfin_compat.sh 12.4        only the versions named
#
#   JFCOMPAT_TREES="a=/src/a b=/src/b"       source trees whose client to test (default: this one);
#                                            each needs compat tests written for its client
#   JFCOMPAT_WORK=/path                      working directory (default: $TMPDIR/jfcompat)
#   JFCOMPAT_FFMPEG=/path/to/ffmpeg          default: /usr/lib/jellyfin-ffmpeg/ffmpeg
#   JFCOMPAT_KEEP=1                          leave the servers and stubs running at the end
#
# Per version it checks:
#   1. every Jellyfin endpoint the orchestrator calls, with the right key and a wrong one
#      (go test -tags integration in internal/jellyfin and cmd/orchestrator, per tree)
#   2. which scheduled tasks open the .strm URLs, and how many bytes they pull. On a real
#      install each of those requests resolves a torrent through the VPN (operations.md §2)
#   3. that playing a .strm reaches its URL, and that the Webhook plugin delivers
#      PlaybackStop with the shared secret
#
# Isolation: each server has its own data, config, cache and log directories under $WORK,
# binds 127.0.0.1 only and has auto-discovery off, so nothing on the LAN finds it. Its
# library holds only test .strm files pointing at its own tests/jellyfin/stub, so it never
# sees a real library and cannot reach an orchestrator, TorrServer or the VPN. Library
# options and disabled tasks mirror a production install (operations.md §5). No root, no
# Docker.
#
# The admin account is disposable: a fixed name and a random password, generated into
# $WORK/credentials on first run.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HERE="$REPO/tests/jellyfin"
WORK="${JFCOMPAT_WORK:-${TMPDIR:-/tmp}/jfcompat}"
FFMPEG="${JFCOMPAT_FFMPEG:-/usr/lib/jellyfin-ffmpeg/ffmpeg}"
TREES="${JFCOMPAT_TREES:-this=$REPO}"
VERSIONS=("$@")
[ ${#VERSIONS[@]} -gt 0 ] || VERSIONS=(10.11.11 12.4)
SERVER_BASE_PORT=18096
STUB_BASE_PORT=19900
ADMIN_USER=jfcompat-admin
WEBHOOK_GUID=71552a5a-5c5c-4350-a2ae-ebe451a30173
# operations.md §5: each of these seeks and probes whole files through the stream URLs.
PROBING_TASKS=("Extract Chapter Images" "Scan Media Library" "Media Segment Scan")
CLIP="$WORK/cache/clip.mp4"

PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf '  \033[1;32m✓\033[0m %s\n' "$1"; }
no(){ FAIL=$((FAIL+1)); printf '  \033[1;31m✗\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }
note(){ printf '      %s\n' "$*"; }
section(){ printf '\n\033[1m%s\033[0m\n' "$*"; }
die(){ printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 2; }
rand(){ head -c 18 /dev/urandom | base64 | tr -d '/+='; }

for t in curl jq go tar ss; do command -v "$t" >/dev/null || die "$t is required"; done
[ -x "$FFMPEG" ] || die "no ffmpeg at $FFMPEG (set JFCOMPAT_FFMPEG)"
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
mkdir -p "$WORK/cache" "$WORK/dist" && chmod 700 "$WORK" || die "cannot use $WORK"

if [ ! -s "$WORK/credentials" ]; then
  ( umask 077; printf 'ADMIN_PASS=%s\nWEBHOOK_SECRET=%s\n' "$(rand)" "$(rand)" > "$WORK/credentials" )
fi
# shellcheck source=/dev/null
. "$WORK/credentials"

PIDS=()
cleanup(){
  if [ "${JFCOMPAT_KEEP:-0}" = 1 ]; then
    [ ${#PIDS[@]} -gt 0 ] && printf '\nleft running (JFCOMPAT_KEEP=1): pids %s\n' "${PIDS[*]}"
    return
  fi
  local p
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
}
trap cleanup EXIT

# ---------------------------------------------------------------- helpers

# api PORT METHOD PATH [TOKEN] [JSON] — print the response body; non-zero on HTTP >= 400.
api(){
  local port=$1 method=$2 path=$3 token=${4:-} body=${5:-}
  local auth='MediaBrowser Client="jfcompat", Device="harness", DeviceId="jfcompat-harness", Version="1.0"'
  [ -n "$token" ] && auth+=", Token=\"$token\""
  local args=(-sS --fail-with-body -m 120 -X "$method" -H "Authorization: $auth")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  curl "${args[@]}" "http://127.0.0.1:$port$path" </dev/null
}

fetch(){ # version — unpack the official portable build into $WORK/dist/<version>
  local v=$1 tgz="$WORK/cache/jellyfin_$1-$ARCH.tar.gz"
  [ -x "$WORK/dist/$v/jellyfin/jellyfin" ] && return 0
  if [ ! -s "$tgz" ]; then
    curl -fsSL --retry 3 -o "$tgz.part" \
      "https://repo.jellyfin.org/files/server/linux/stable/v$v/$ARCH/jellyfin_$v-$ARCH.tar.gz" || return 1
    mv "$tgz.part" "$tgz"
  fi
  mkdir -p "$WORK/dist/$v" && tar -xzf "$tgz" -C "$WORK/dist/$v"
}

make_clip(){ # a 2-minute 720p clip, big enough to tell a header probe from a full read
  [ -s "$CLIP" ] && return 0
  nice -n 19 "$FFMPEG" -nostdin -loglevel error \
    -f lavfi -i testsrc2=size=1280x720:rate=24:duration=120 -f lavfi -i sine=frequency=440:duration=120 \
    -c:v libx264 -preset ultrafast -b:v 6M -pix_fmt yuv420p -c:a aac -b:a 128k -movflags +faststart \
    -y "$CLIP.part.mp4" && mv "$CLIP.part.mp4" "$CLIP"
}

seed(){ # dir port — directories, and a network config that keeps the server on loopback
  rm -rf "$1" && mkdir -p "$1"/data "$1"/config "$1"/cache "$1"/log || return 1
  cat > "$1/config/network.xml" <<EOF
<?xml version="1.0" encoding="utf-8"?>
<NetworkConfiguration xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <InternalHttpPort>$2</InternalHttpPort>
  <PublicHttpPort>$2</PublicHttpPort>
  <AutoDiscovery>false</AutoDiscovery>
  <EnableIPv6>false</EnableIPv6>
  <EnableRemoteAccess>false</EnableRemoteAccess>
  <LocalNetworkAddresses>
    <string>127.0.0.1</string>
  </LocalNetworkAddresses>
</NetworkConfiguration>
EOF
}

port_free(){ ! ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }

start_server(){ # dir version port
  local dir=$1 port=$3 pid i
  # A server left on this port would answer every check below in place of this one.
  port_free "$port" || { echo "port $port is already in use" >&2; return 1; }
  ( cd "$dir" && exec "$WORK/dist/$2/jellyfin/jellyfin" --datadir "$dir/data" --configdir "$dir/config" \
      --cachedir "$dir/cache" --logdir "$dir/log" --ffmpeg "$FFMPEG" ) >>"$dir/console.log" 2>&1 &
  pid=$!
  echo "$pid" > "$dir/pid"
  PIDS+=("$pid")
  # Ready means this process owns the port and reports a version: while starting or
  # migrating, Jellyfin answers /System/Info/Public with 200 and "Version": null, then
  # 503 to everything else until it is done.
  for i in $(seq 1 900); do
    kill -0 "$pid" 2>/dev/null || { tail -n 20 "$dir/console.log" >&2; return 1; }
    if ss -Hltnp "sport = :$port" 2>/dev/null | grep -q "pid=$pid," &&
       curl -s -m 2 "http://127.0.0.1:$port/System/Info/Public" 2>/dev/null | jq -e '.Version != null' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

stop_server(){ # dir
  local pid i
  pid=$(cat "$1/pid" 2>/dev/null) || return 0
  kill "$pid" 2>/dev/null
  for i in $(seq 1 60); do kill -0 "$pid" 2>/dev/null || return 0; sleep 1; done
  kill -9 "$pid" 2>/dev/null
}

start_stub(){ # dir port server-port — the key file is written once the server has a key
  port_free "$2" || { echo "port $2 is already in use" >&2; return 1; }
  "$WORK/stub" -addr "127.0.0.1:$2" -clip "$CLIP" -log "$1/stub.jsonl" -secret "$WEBHOOK_SECRET" \
    -jellyfin "http://127.0.0.1:$3" -keyfile "$1/stub.key" >>"$1/stub.console.log" 2>&1 &
  PIDS+=("$!")
  local i
  for i in $(seq 1 20); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' -m 2 "http://127.0.0.1:$2/")" = 404 ] && return 0
    sleep 0.5
  done
  return 1
}

check_isolation(){ # pid
  local socks
  socks=$(ss -Hltnup 2>/dev/null | grep "pid=$1,")
  if [ -z "$socks" ]; then no "could not list the server's sockets"; return; fi
  if awk '{print $5}' <<<"$socks" | grep -q -v '^127\.0\.0\.1:'; then
    no "server listens beyond loopback" "$(awk '{print $1" "$5}' <<<"$socks" | tr '\n' ' ')"
  else
    ok "listens on loopback only: $(awk '{print $1" "$5}' <<<"$socks" | sort -u | tr '\n' ' ')"
  fi
}

# Stub log accounting: mark before a step, hits after it.
mark(){ wc -l < "$STUB_LOG" 2>/dev/null || echo 0; }
since(){ tail -n +"$(( $1 + 1 ))" "$STUB_LOG"; }
hits(){ # mark — "N requests, X MiB" of /play/ traffic since the mark
  since "$1" | jq -rs '[.[] | select(.kind=="play")] |
    "\(length) requests, \(((map(.bytes) | add // 0) / 1048576 * 10 | floor) / 10) MiB"'
}
hit_count(){ since "$1" | jq -s '[.[] | select(.kind=="play")] | length'; }
agents(){ since "$1" | jq -rs '[.[] | select(.kind=="play") | .ua] | unique | join(", ")'; }

run_task(){ # port key id — start a task, wait for it to finish, print its result
  local p=$1 k=$2 id=$3 before state end status i
  before=$(api "$p" GET "/ScheduledTasks/$id" "$k" | jq -r '.LastExecutionResult.EndTimeUtc // "-"')
  api "$p" POST "/ScheduledTasks/Running/$id" "$k" >/dev/null || { echo "could-not-start"; return 1; }
  for i in $(seq 1 600); do
    sleep 1
    read -r state end status < <(api "$p" GET "/ScheduledTasks/$id" "$k" |
      jq -r '"\(.State) \(.LastExecutionResult.EndTimeUtc // "-") \(.LastExecutionResult.Status // "-")"')
    if [ "$state" = Idle ] && [ "$end" != "$before" ]; then echo "$status"; return 0; fi
  done
  echo "timeout"
  return 1
}

make_library(){ # dir stubport — the shape the orchestrator writes: a folder per movie, seasons for shows
  local lib=$1/library url="http://127.0.0.1:$2/play"
  mkdir -p "$lib/movies/Big Buck Bunny (2008)" "$lib/movies/Sintel (2010)" "$lib/tv/Pioneer One (2010)/Season 01"
  printf '%s/movie/10378?t=jfcompat' "$url" > "$lib/movies/Big Buck Bunny (2008)/Big Buck Bunny (2008).strm"
  printf '%s/movie/45745?t=jfcompat' "$url" > "$lib/movies/Sintel (2010)/Sintel (2010).strm"
  printf '%s/tv/31931/1/1?t=jfcompat' "$url" > "$lib/tv/Pioneer One (2010)/Season 01/Pioneer One (2010) S01E01.strm"
  printf '%s/tv/31931/1/2?t=jfcompat' "$url" > "$lib/tv/Pioneer One (2010)/Season 01/Pioneer One (2010) S01E02.strm"
  # Titles TMDB cannot match get no artwork from it, which is when image fetchers that read
  # the file itself (Screen Grabber, Embedded Image Extractor) would have to step in.
  mkdir -p "$lib/movies/Zzqx Unmatched Film (2031)" "$lib/tv/Qqzx Unknown Show (2031)/Season 01"
  printf '%s/movie/999999991?t=jfcompat' "$url" > "$lib/movies/Zzqx Unmatched Film (2031)/Zzqx Unmatched Film (2031).strm"
  printf '%s/tv/999999992/1/1?t=jfcompat' "$url" > "$lib/tv/Qqzx Unknown Show (2031)/Season 01/Qqzx Unknown Show (2031) S01E01.strm"
}

apply_library_options(){ # dir — production options from fixtures/, with this sandbox's paths
  local name fixture opts
  for name in Movies Shows; do
    fixture="$HERE/fixtures/$(tr '[:upper:]' '[:lower:]' <<<"$name").options.xml"
    opts="$1/data/root/default/$name/options.xml"
    [ -f "$opts" ] || return 1
    local path=$1/library/movies
    [ "$name" = Shows ] && path=$1/library/tv
    sed "s#@LIBRARY@#$path#" "$fixture" > "$opts" || return 1
  done
}

configure_webhook(){ # port token stubport — PlaybackStop for movies and episodes, with the secret
  local id tmpl cfg
  id=$(api "$1" GET /Plugins "$2" | jq -r '.[] | select(.Name=="Webhook") | .Id') || return 1
  [ -n "$id" ] || return 1
  # Only the fields the orchestrator reads (internal/jellyfin WebhookPayload), all strings.
  tmpl=$(printf '%s' '{"NotificationType":"{{NotificationType}}","ItemId":"{{ItemId}}","ItemType":"{{ItemType}}"}' | base64 -w0)
  cfg=$(api "$1" GET "/Plugins/$id/Configuration" "$2" | jq -c \
    --arg uri "http://127.0.0.1:$3/webhook/jellyfin" --arg t "$tmpl" --arg s "$WEBHOOK_SECRET" '
    .GenericOptions = [{NotificationTypes: ["PlaybackStop"], WebhookName: "jellyfreedom", WebhookUri: $uri,
      EnableMovies: true, EnableEpisodes: true, EnableSeries: false, EnableSeasons: false,
      EnableAlbums: false, EnableSongs: false, EnableVideos: true, SendAllProperties: false,
      TrimWhitespace: false, SkipEmptyMessageBody: false, EnableWebhook: true, Template: $t,
      UserFilter: [], Headers: [{Key: "X-JellyFreedom-Token", Value: $s}], Fields: []}]') || return 1
  api "$1" POST "/Plugins/$id/Configuration" "$2" "$cfg" >/dev/null
}

run_go_tests(){ # label tree port key item strm out
  local label=$1 tree=$2 out=$7
  ( cd "$tree" && JF_COMPAT_URL="http://127.0.0.1:$3" JF_COMPAT_KEY="$4" \
      JF_COMPAT_USER="$ADMIN_USER" JF_COMPAT_PASS="$ADMIN_PASS" \
      JF_COMPAT_ITEM="$5" JF_COMPAT_STRM="$6" JF_COMPAT_SCAN=1 \
      go test -tags integration -count=1 -json -run '^TestCompat' ./internal/jellyfin/ ./cmd/orchestrator/ \
  ) > "$out" 2> "${out%.json}.err"
  local test action msg
  while read -r action test; do
    case "$action" in
      pass) ok "[$label] $test" ;;
      skip) no "[$label] $test skipped" ;;
      fail)
        msg=$(jq -r --arg t "$test" 'select(.Test==$t and .Action=="output") | .Output' "$out" |
          grep -E 'compat_test.go:[0-9]+:' | sed -E 's/^ +//' | head -n 3 | tr '\n' ' ')
        no "[$label] $test" "$msg" ;;
    esac
  done < <(jq -r 'select(.Test != null and (.Action=="pass" or .Action=="fail" or .Action=="skip")) | "\(.Action) \(.Test)"' "$out")
  if ! jq -e 'select(.Test != null)' "$out" >/dev/null 2>&1; then
    no "[$label] tests did not run" "$(head -n 5 "${out%.json}.err" | tr '\n' ' ')"
  fi
}

# The PlaybackStop handler drops a torrent only when no other session is playing the item,
# so it depends on Jellyfin having cleared the stopping session by the time the webhook
# arrives, while still listing everyone else. The stub asks /Sessions at that moment.
viewer(){ # port device — log in as the admin on a device of its own; print the token
  curl -sS -m 30 -X POST -H "Authorization: MediaBrowser Client=\"jfcompat\", Device=\"$2\", DeviceId=\"$2\", Version=\"1.0\"" \
    -H 'Content-Type: application/json' --data "$(jq -nc --arg u "$ADMIN_USER" --arg p "$ADMIN_PASS" '{Username:$u, Pw:$p}')" \
    "http://127.0.0.1:$1/Users/AuthenticateByName" </dev/null | jq -r .AccessToken
}
report(){ # port device token endpoint item
  curl -sS -m 30 -o /dev/null -X POST \
    -H "Authorization: MediaBrowser Client=\"jfcompat\", Device=\"$2\", DeviceId=\"$2\", Version=\"1.0\", Token=\"$3\"" \
    -H 'Content-Type: application/json' --data "$(jq -nc --arg i "$5" --arg s "jfcompat-$2" \
      '{ItemId:$i, PlaySessionId:$s, PositionTicks:50000000, CanSeek:true, IsPaused:false, PlayMethod:"DirectPlay"}')" \
    "http://127.0.0.1:$1$4" </dev/null
}
seen_at_webhook(){ since "$1" | jq -rs '[.[] | select(.kind=="webhook")] | last | .playing_at_webhook // "none"'; }
two_viewers(){ # port item
  local a b m first last
  a=$(viewer "$1" jfcompat-viewer-a)
  b=$(viewer "$1" jfcompat-viewer-b)
  report "$1" jfcompat-viewer-a "$a" /Sessions/Playing "$2"
  report "$1" jfcompat-viewer-b "$b" /Sessions/Playing "$2"
  sleep 2
  m=$(mark); report "$1" jfcompat-viewer-a "$a" /Sessions/Playing/Stopped "$2"; sleep 4; first=$(seen_at_webhook "$m")
  m=$(mark); report "$1" jfcompat-viewer-b "$b" /Sessions/Playing/Stopped "$2"; sleep 4; last=$(seen_at_webhook "$m")
  if [ "$first" = 1 ] && [ "$last" = 0 ]; then
    ok "two viewers: when one stops the webhook still sees the other (1); when the last stops, nobody (0)"
  else
    no "two viewers: sessions seen at webhook time were $first then $last; want 1 then 0"
  fi
}

# ---------------------------------------------------------------- setup

section "Setup"
go build -o "$WORK/stub" "$HERE/stub" || die "could not build the stub"
make_clip || die "could not make the test clip with $FFMPEG"
ok "stub built, test clip $(du -h "$CLIP" | cut -f1)"
declare -A TREE
for pair in $TREES; do
  label=${pair%%=*}
  tree=${pair#*=}
  [ -f "$tree/internal/jellyfin/compat_test.go" ] || die "tree $label ($tree) has no internal/jellyfin/compat_test.go"
  ( cd "$tree" && go vet -tags integration ./internal/jellyfin/ ./cmd/orchestrator/ ) || die "tree $label does not build"
  TREE[$label]=$tree
  ok "tree $label: $tree ($(git -C "$tree" log -1 --format='%h %s' 2>/dev/null | cut -c1-60))"
done

# ---------------------------------------------------------------- per version

i=0
for V in "${VERSIONS[@]}"; do
  PORT=$((SERVER_BASE_PORT + i))
  SPORT=$((STUB_BASE_PORT + i))
  i=$((i + 1))
  DIR="$WORK/srv-$V"
  STUB_LOG="$DIR/stub.jsonl"
  section "Jellyfin $V  (server 127.0.0.1:$PORT, stub 127.0.0.1:$SPORT)"

  fetch "$V" || { no "could not fetch Jellyfin $V"; continue; }
  seed "$DIR" "$PORT" || { no "could not create $DIR"; continue; }
  make_library "$DIR" "$SPORT"
  : > "$STUB_LOG"
  start_stub "$DIR" "$SPORT" "$PORT" || { no "stub did not start"; continue; }
  if ! start_server "$DIR" "$V" "$PORT"; then no "server did not start (see $DIR/console.log)"; continue; fi
  got=$(curl -s "http://127.0.0.1:$PORT/System/Info/Public" | jq -r .Version)
  ok "server up, reports version $got"
  check_isolation "$(cat "$DIR/pid")"

  # -- first-run wizard, disposable admin, API key
  api "$PORT" POST /Startup/Configuration "" \
    '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' >/dev/null &&
  api "$PORT" GET /Startup/User >/dev/null &&
  api "$PORT" POST /Startup/User "" "$(jq -nc --arg n "$ADMIN_USER" --arg p "$ADMIN_PASS" '{Name:$n, Password:$p}')" >/dev/null &&
  { api "$PORT" POST /Startup/RemoteAccess "" '{"EnableRemoteAccess":false,"EnableAutomaticPortMapping":false}' >/dev/null 2>&1; true; } &&
  api "$PORT" POST /Startup/Complete >/dev/null || { no "first-run wizard failed"; continue; }
  read -r TOKEN USER_ID < <(api "$PORT" POST /Users/AuthenticateByName "" \
    "$(jq -nc --arg u "$ADMIN_USER" --arg p "$ADMIN_PASS" '{Username:$u, Pw:$p}')" | jq -r '"\(.AccessToken) \(.User.Id)"')
  [ -n "${TOKEN:-}" ] && [ "$TOKEN" != null ] || { no "could not log in as $ADMIN_USER"; continue; }
  api "$PORT" POST "/Auth/Keys?app=jfcompat" "$TOKEN" >/dev/null
  KEY=$(api "$PORT" GET /Auth/Keys "$TOKEN" | jq -r '[.Items[] | select(.AppName=="jfcompat")][0].AccessToken')
  [ -n "$KEY" ] && [ "$KEY" != null ] || { no "could not create an API key"; continue; }
  ok "wizard done; disposable admin $ADMIN_USER and an API key created"
  ( umask 077; printf 'PORT=%s\nKEY=%s\nTOKEN=%s\nUSER_ID=%s\n' "$PORT" "$KEY" "$TOKEN" "$USER_ID" > "$DIR/session.env"
    printf '%s' "$KEY" > "$DIR/stub.key" )
  legacy=$(grep -o '<EnableLegacyAuthorization>[a-z]*' "$DIR/config/system.xml" 2>/dev/null | cut -d'>' -f2)
  note "EnableLegacyAuthorization: ${legacy:-not present}"

  # -- libraries, the Webhook plugin, then a restart so both take effect
  api "$PORT" POST "/Library/VirtualFolders?name=Movies&collectionType=movies&refreshLibrary=false" "$KEY" \
    "$(jq -nc --arg p "$DIR/library/movies" '{LibraryOptions:{PathInfos:[{Path:$p}]}}')" >/dev/null &&
  api "$PORT" POST "/Library/VirtualFolders?name=Shows&collectionType=tvshows&refreshLibrary=false" "$KEY" \
    "$(jq -nc --arg p "$DIR/library/tv" '{LibraryOptions:{PathInfos:[{Path:$p}]}}')" >/dev/null ||
    { no "could not add the libraries"; continue; }
  api "$PORT" POST "/Packages/Installed/Webhook?assemblyGuid=$WEBHOOK_GUID" "$KEY" >/dev/null ||
    no "could not ask the server to install the Webhook plugin"
  for _ in $(seq 1 90); do compgen -G "$DIR/data/plugins/Webhook_*/*.dll" >/dev/null && break; sleep 1; done
  stop_server "$DIR"
  if apply_library_options "$DIR"; then opts_copied=1; else opts_copied=0; fi
  start_server "$DIR" "$V" "$PORT" || { no "server did not come back after the restart"; continue; }

  plugin=$(api "$PORT" GET /Plugins "$KEY" | jq -r '.[] | select(.Name=="Webhook") | "\(.Version) \(.Status)"')
  case "$plugin" in
    *" Active") ok "Webhook plugin $plugin" ;;
    "") no "Webhook plugin is not installed" ;;
    *) no "Webhook plugin $plugin" ;;
  esac
  libs=$(api "$PORT" GET /Library/VirtualFolders "$KEY")
  if [ "$opts_copied" = 1 ] && jq -e '
      (.[] | select(.Name=="Movies") | .LibraryOptions |
        .AutomaticallyAddToCollection == true and .EnableTrickplayImageExtraction == true and
        ([.TypeOptions[] | select(.Type=="Movie") | .MetadataFetchers[]] == ["TheMovieDb","The Open Movie Database"]))
      and (.[] | select(.Name=="Shows") | .LibraryOptions | .EnableTrickplayImageExtraction == false)' \
      <<<"$libs" >/dev/null; then
    ok "production library options applied (fixtures/*.options.xml)"
  else
    no "production library options did not take" "copied=$opts_copied; the probe numbers below use Jellyfin's defaults"
  fi
  if [ -n "$plugin" ]; then
    configure_webhook "$PORT" "$KEY" "$SPORT" && ok "webhook → stub, PlaybackStop, X-JellyFreedom-Token" ||
      no "could not configure the webhook"
  fi

  # -- scheduled tasks: report the defaults, then disable the three production disables
  tasks=$(api "$PORT" GET /ScheduledTasks "$KEY")
  note "tasks with default triggers: $(jq -r '[.[] | select((.Triggers | length) > 0) | .Name] | sort | join(", ")' <<<"$tasks")"
  for name in "${PROBING_TASKS[@]}"; do
    tid=$(jq -r --arg n "$name" '.[] | select(.Name==$n) | .Id' <<<"$tasks")
    if [ -z "$tid" ]; then no "no task named \"$name\" — the production setup cannot disable it"; continue; fi
    api "$PORT" POST "/ScheduledTasks/$tid/Triggers" "$KEY" '[]' >/dev/null &&
      ok "disabled \"$name\"" || no "could not disable \"$name\""
  done

  # -- the scan, and what it pulled
  scan_id=$(jq -r '.[] | select(.Key=="RefreshLibrary") | .Id' <<<"$tasks")
  m=$(mark)
  r=$(run_task "$PORT" "$KEY" "$scan_id")
  sleep 20
  scan_hits=$(hit_count "$m")
  [ "$scan_hits" = 0 ] && ok "library scan ($r) opened no stream URL" ||
    no "library scan ($r) opened the stream URLs: $(hits "$m")" "user agents: $(agents "$m")"
  items=$(api "$PORT" GET "/Items?userId=$USER_ID&Recursive=true&IncludeItemTypes=Movie,Episode&Fields=Path,MediaSources&SortBy=SortName" "$KEY")
  found=$(jq -r '[.Items[] | .Name] | join(", ")' <<<"$items")
  [ "$(jq '.Items | length' <<<"$items")" = 6 ] && ok "all 6 .strm items in the library: $found" ||
    no "expected 6 items after the scan" "found: ${found:-none}"
  ITEM=$(jq -r '[.Items[] | select(.Type=="Movie" and .Name=="Big Buck Bunny")][0].Id // empty' <<<"$items")
  STRM=$(jq -r '[.Items[] | select(.Type=="Movie" and .Name=="Big Buck Bunny")][0].Path // empty' <<<"$items")
  note "movie $ITEM: Path=$STRM"
  note "        MediaSources[0]: $(jq -c '[.Items[] | select(.Type=="Movie")][0].MediaSources[0] | {Protocol, IsRemote, Path}' <<<"$items")"

  # -- every other scheduled task, one at a time
  printf '      %-42s %-10s %s\n' "task" "result" "stream traffic"
  while IFS=$'\t' read -r tid tkey tname; do
    case "$tkey" in RefreshLibrary|PluginUpdates) continue ;; esac
    m=$(mark)
    r=$(run_task "$PORT" "$KEY" "$tid")
    sleep 3
    printf '      %-42s %-10s %s\n' "${tname:0:42}" "$r" "$(hits "$m")"
    if [ "$(hit_count "$m")" != 0 ]; then
      no "task \"$tname\" opened the stream URLs: $(hits "$m")" "user agents: $(agents "$m")"
    fi
  done < <(jq -r '.[] | [.Id, .Key, .Name] | @tsv' <<<"$tasks")

  # -- playback: what a client does
  if [ -n "$ITEM" ]; then
    m=$(mark)
    pinfo=$(api "$PORT" POST "/Items/$ITEM/PlaybackInfo?userId=$USER_ID" "$TOKEN" '{}')
    MSID=$(jq -r '.MediaSources[0].Id // empty' <<<"$pinfo")
    note "PlaybackInfo: $(jq -c '.MediaSources[0] | {Protocol, SupportsDirectPlay, SupportsDirectStream, Container, RunTimeTicks}' <<<"$pinfo") — $(hits "$m")"
    m=$(mark)
    res=$(curl -sS -o /dev/null -m 60 -r 0-1048575 -w '%{http_code} %{size_download}' \
      -H "Authorization: MediaBrowser Client=\"jfcompat\", Device=\"harness\", DeviceId=\"jfcompat-harness\", Version=\"1.0\", Token=\"$TOKEN\"" \
      "http://127.0.0.1:$PORT/Videos/$ITEM/stream?static=true&mediaSourceId=$MSID")
    if [ "${res%% *}" = 206 ] || [ "${res%% *}" = 200 ]; then
      [ "$(hit_count "$m")" -gt 0 ] && ok "playing the .strm reaches its URL (HTTP $res bytes; stub: $(hits "$m"))" ||
        no "stream answered HTTP $res without touching the stub"
    else
      no "static stream of the .strm failed: HTTP $res"
    fi
  fi

  # -- the client itself, per source tree; the session test fires the webhook
  m=$(mark)
  for label in "${!TREE[@]}"; do
    run_go_tests "$label" "${TREE[$label]}" "$PORT" "$KEY" "$ITEM" "$STRM" "$DIR/gotest-$label.json"
  done
  if [ -n "$plugin" ]; then
    sleep 3
    wh=$(since "$m" | jq -sc '[.[] | select(.kind=="webhook")]')
    n=$(jq length <<<"$wh")
    if [ "$n" -gt 0 ] && jq -e --arg id "$ITEM" 'all(.[]; .token_ok == true and .body.NotificationType == "PlaybackStop"
        and (.body.ItemId | ascii_downcase | gsub("-"; "")) == ($id | ascii_downcase | gsub("-"; ""))
        and .playing_at_webhook == 0)' <<<"$wh" >/dev/null; then
      ok "webhook delivered $n PlaybackStop call(s) with the secret, the right ItemId ($(jq -r '.[0].body.ItemId' <<<"$wh")), and nobody left playing"
    else
      no "webhook: $n call(s), not all PlaybackStop with the secret, the right ItemId and nobody left playing" \
        "$(jq -c '[.[] | {token_ok, playing_at_webhook, body}]' <<<"$wh" | cut -c1-300)"
    fi
    two_viewers "$PORT" "$ITEM"
  fi

  errs=$(cat "$DIR"/log/*.log 2>/dev/null | grep -c -E '\[(ERR|FTL)\]')
  note "server log: ${errs:-0} ERR/FTL lines — $DIR/log/"
  note "ffmpeg: $(grep -h -o -m1 -E 'FFmpeg: [^"]*|ffmpeg version [^ ]+|Using FFmpeg[^"]*' "$DIR"/log/*.log 2>/dev/null | head -n 1)"
  [ "${JFCOMPAT_KEEP:-0}" = 1 ] || stop_server "$DIR"
done

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
