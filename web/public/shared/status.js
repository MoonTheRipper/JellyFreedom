/* ==========================================================================
   Setup/config state shared by the public app and the dashboard.

   GET /api/configured has existed since the beginning and its own code comment
   says "the media UI shows a banner" — no UI has ever called it. That is why a
   fresh install renders a completely blank home page: no hero, no carousels, a
   "Search failed." hint, and nothing anywhere telling the user to add a TMDB
   key. This module is the single place that answers "what is missing?".
   ========================================================================== */

import {apiTry} from './api.js';

/** Where a new user gets each missing piece. */
export const HELP_LINKS = {
  tmdb: {label: 'Get a free TMDB API key', url: 'https://www.themoviedb.org/settings/api'},
  prowlarr_indexers: {label: 'Prowlarr indexer docs', url: 'https://wiki.servarr.com/prowlarr/indexers'},
  jellyfin_library: {label: 'Jellyfin library setup', url: 'https://jellyfin.org/docs/general/server/libraries/'},
  wireguard: {label: 'WireGuard config help', url: 'https://www.wireguard.com/quickstart/'},
};

/** Shape returned when the endpoint is missing or unreachable. Unknown is NOT
 *  "broken": we must not shout "configure TMDB" at someone whose install is
 *  fine but whose build predates the extended endpoint. */
const UNKNOWN = {
  known: false, extended: false,
  tmdb: true, prowlarr: true, jellyfin: true, torrserver: true,
  vpn_configured: true, vpn_active: true,
  indexer_count: -1, jellyfin_url: '', setup_complete: true,
};

let cache = null;

/**
 * loadConfigured reads GET /api/configured (public, contract §5).
 * Tolerates the pre-contract response, which only had the four booleans.
 */
export async function loadConfigured({force = false} = {}) {
  if (cache && !force) return cache;
  const r = await apiTry('/api/configured');
  if (!r.ok || !r.data || typeof r.data !== 'object') {
    cache = {...UNKNOWN};
    return cache;
  }
  const d = r.data;
  const has = k => Object.prototype.hasOwnProperty.call(d, k);
  const cfg = {
    known: true,
    tmdb: !!d.tmdb,
    prowlarr: !!d.prowlarr,
    jellyfin: !!d.jellyfin,
    torrserver: !!d.torrserver,
    // Fields below arrived with contract v0.3. On an older build they are
    // absent — treat absent as "cannot tell", never as "broken".
    vpn_configured: has('vpn_configured') ? !!d.vpn_configured : true,
    vpn_active: has('vpn_active') ? !!d.vpn_active : true,
    indexer_count: has('indexer_count') ? Number(d.indexer_count) : -1,
    jellyfin_url: typeof d.jellyfin_url === 'string' ? d.jellyfin_url : '',
    setup_complete: has('setup_complete') ? !!d.setup_complete : undefined,
    extended: has('setup_complete'),
  };
  if (cfg.setup_complete === undefined) cfg.setup_complete = issues(cfg).every(i => !i.blocking);
  // UNKNOWN (endpoint missing) is treated as "cannot tell", never as broken.
  cache = cfg;
  return cfg;
}

export function cachedConfigured() { return cache; }

/**
 * issues turns the config into an ordered checklist. Each row:
 *   key, label, state ('ok' | 'bad' | 'warn' | 'unknown'), detail,
 *   blocking (true = the app cannot work at all), fix ({hash} or {url}).
 */
export function issues(cfg) {
  const c = cfg || UNKNOWN;
  const rows = [];

  rows.push({
    key: 'tmdb',
    label: 'TMDB API key',
    state: c.tmdb ? 'ok' : 'bad',
    blocking: !c.tmdb,
    detail: c.tmdb
      ? 'Metadata, posters and search are working.'
      : 'Without it there is no search, no artwork and no browse rows — the home page stays empty.',
    fix: {hash: '#settings', label: 'Add the key in Settings → Connections', external: HELP_LINKS.tmdb},
  });

  // Prowlarr has TWO failure modes and the second one is invisible: a valid
  // key with zero indexers returns an empty list forever, which the old UI
  // rendered as "No releases found".
  if (!c.prowlarr) {
    rows.push({
      key: 'prowlarr',
      label: 'Prowlarr connection',
      state: 'bad', blocking: true,
      detail: 'No indexer source configured, so nothing can ever be found to stream.',
      fix: {hash: '#settings', label: 'Add the Prowlarr URL and key in Settings → Connections'},
    });
  } else if (c.indexer_count === 0) {
    rows.push({
      key: 'prowlarr',
      label: 'Prowlarr indexers',
      state: 'bad', blocking: true,
      detail: 'Prowlarr is connected but has ZERO indexers configured. Every search will succeed and return nothing, forever. Add at least one indexer in Prowlarr itself.',
      fix: {hash: '#settings', label: 'Open Prowlarr and add an indexer', external: HELP_LINKS.prowlarr_indexers},
    });
  } else {
    rows.push({
      key: 'prowlarr',
      label: 'Prowlarr indexers',
      state: c.indexer_count > 0 ? 'ok' : 'unknown',
      blocking: false,
      detail: c.indexer_count > 0
        ? `${c.indexer_count} indexer${c.indexer_count === 1 ? '' : 's'} configured.`
        : 'Connected. Indexer count not reported by this build.',
      fix: {hash: '#settings', label: 'Settings → Connections'},
    });
  }

  rows.push({
    key: 'jellyfin',
    label: 'Jellyfin',
    state: c.jellyfin ? 'ok' : 'warn',
    blocking: false,
    detail: c.jellyfin
      ? 'Connected. Requested titles appear in your Jellyfin libraries.'
      : 'Not connected. Requests still resolve, but nothing will show up in a player and library refreshes will not fire.',
    fix: {hash: '#settings', label: 'Settings → Connections', external: HELP_LINKS.jellyfin_library},
  });

  rows.push({
    key: 'torrserver',
    label: 'TorrServer',
    state: c.torrserver ? 'ok' : 'bad',
    blocking: !c.torrserver,
    detail: c.torrserver
      ? 'Streaming engine reachable.'
      : 'The streaming engine is unreachable, so nothing can play even once a release is picked.',
    fix: {hash: '#health', label: 'Check the service in Health'},
  });

  // A build that predates contract v0.3 reports no VPN fields at all. Saying
  // "ok" there would be an outright lie about the one thing that must fail
  // closed, so it is reported as unknown instead.
  if (!c.extended) {
    rows.push({
      key: 'vpn',
      label: 'VPN tunnel',
      state: 'unknown',
      blocking: false,
      detail: 'This build does not report VPN state to the UI. Check it on the VPN page — torrent traffic is blocked while the tunnel is down.',
      fix: {hash: '#vpn', label: 'VPN → Configurations', external: HELP_LINKS.wireguard},
    });
  } else {
    rows.push({
      key: 'vpn',
      label: 'VPN tunnel',
      state: !c.vpn_configured ? 'bad' : (!c.vpn_active ? 'warn' : 'ok'),
      blocking: !c.vpn_configured,
      detail: !c.vpn_configured
        ? 'No WireGuard config uploaded. Torrent traffic is blocked by design until one is active — every request will fail.'
        : (!c.vpn_active
          ? 'A config exists but the tunnel is DOWN. Requests queued now will fail until it comes back up.'
          : 'Tunnel up. Torrent traffic is confined to the VPN namespace.'),
      fix: {hash: '#vpn', label: 'VPN → Configurations', external: HELP_LINKS.wireguard},
    });
  }

  return rows;
}

/** blockingIssues returns only the rows that stop the app working. */
export function blockingIssues(cfg) {
  return issues(cfg).filter(i => i.blocking);
}

/**
 * loadHealth reads GET /api/health/summary (public, contract §4):
 *   {"ok": bool, "degraded": ["tmdb","prowlarr","vpn"]}
 * Returns {known, ok, degraded[]}. If the route is absent (older build, or
 * swallowed by the admin-only /api/ catch-all) we report unknown rather than
 * painting a red dot at everyone.
 */
export const HEALTH_COMPONENTS = ['tmdb', 'prowlarr', 'jellyfin', 'torrserver', 'vpn'];
const COMPONENT_LABEL = {
  tmdb: 'TMDB', prowlarr: 'Prowlarr', jellyfin: 'Jellyfin',
  torrserver: 'TorrServer', vpn: 'VPN tunnel',
};
export function componentLabel(k) { return COMPONENT_LABEL[k] || k; }

export async function loadHealth() {
  const r = await apiTry('/api/health/summary');
  if (!r.ok || !r.data || typeof r.data !== 'object') {
    return {known: false, ok: true, degraded: []};
  }
  const degraded = Array.isArray(r.data.degraded)
    ? r.data.degraded.filter(x => HEALTH_COMPONENTS.includes(x))
    : [];
  return {known: true, ok: r.data.ok !== false && degraded.length === 0, degraded};
}
