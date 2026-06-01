package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
)

// Services that the dashboard monitors.
var managedServices = []string{
	"jellyfin",
	"vpntorrent-netns",
	"torrserver-netns",
	"prowlarr",
	"flaresolverr",
	"jellyfreedom",
}

// knownPorts maps service name to its expected port (for lookup).
var knownPorts = map[string]string{
	"jellyfin":         "8096",
	"torrserver-netns": "8090",
	"prowlarr":         "9696",
	"flaresolverr":     "8191",
	"jellyfreedom":     "1990",
}

type ServiceStatus struct {
	Name    string `json:"name"`
	Active  string `json:"active"`
	Sub     string `json:"sub"`
	Since   string `json:"since"`
	Enabled bool   `json:"enabled"`
	Bind    string `json:"bind,omitempty"` // e.g. "127.0.0.1:8090" or "0.0.0.0:9696"
}

type VPNStatus struct {
	Interface    string    `json:"interface"`
	Connected    bool      `json:"connected"`
	PeerKey      string    `json:"peer_key"`
	Endpoint     string    `json:"endpoint"`
	HandshakeAge string    `json:"handshake_age"`
	HandshakeAt  time.Time `json:"handshake_at,omitempty"`
	RxBytes      int64     `json:"rx_bytes"`
	TxBytes      int64     `json:"tx_bytes"`
	ExitIP       string    `json:"exit_ip,omitempty"`
}

// StatusHandler returns health of all managed services + VPN.
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	var statuses []ServiceStatus
	for _, svc := range managedServices {
		statuses = append(statuses, getServiceStatus(svc))
	}
	vpn := getVPNStatus()
	jsonOK(w, map[string]any{
		"services":  statuses,
		"vpn":       vpn,
		"timestamp": time.Now().Unix(),
	})
}

// LogsHandler returns recent journal lines for a service.
func LogsHandler(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("svc")
	if svc == "" {
		jsonErr(w, "svc required", http.StatusBadRequest)
		return
	}
	// Validate against known services
	allowed := false
	for _, s := range managedServices {
		if s == svc {
			allowed = true
			break
		}
	}
	if !allowed {
		jsonErr(w, "unknown service", http.StatusBadRequest)
		return
	}
	n := 80
	if nStr := r.URL.Query().Get("n"); nStr != "" {
		if parsed, err := strconv.Atoi(nStr); err == nil && parsed > 0 && parsed <= 500 {
			n = parsed
		}
	}
	lines := journalLines(svc, n)
	jsonOK(w, map[string]any{"service": svc, "lines": lines})
}

// ServiceRestartHandler restarts a named systemd service.
func ServiceRestartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Extract service name from path: /api/services/{name}/restart
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		jsonErr(w, "bad path", http.StatusBadRequest)
		return
	}
	svc := parts[len(parts)-2]
	allowed := false
	for _, s := range managedServices {
		if s == svc {
			allowed = true
			break
		}
	}
	if !allowed {
		jsonErr(w, "unknown service", http.StatusBadRequest)
		return
	}
	out, err := sudoSystemctl("restart", svc)
	if err != nil {
		jsonErr(w, fmt.Sprintf("restart failed: %s %s", err, out), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "restarted", "service": svc})
}

// VPNHandler returns WireGuard status and available configs.
func VPNHandler(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, getVPNStatus())
}

// VPN config management (upload/list/activate/delete/download) lives in the orchestrator
// (cmd/orchestrator) against the orchestrator-owned config dir; the old /etc/wireguard-based
// "switch" handler was removed in favour of that. See DECISIONS.md D14.

// --- helpers ---

func getServiceStatus(name string) ServiceStatus {
	s := ServiceStatus{Name: name, Active: "unknown"}
	out, err := exec.Command("systemctl", "show", name,
		"--property=ActiveState,SubState,ActiveEnterTimestamp,UnitFileState").Output()
	if err != nil {
		return s
	}
	props := parseProps(string(out))
	s.Active = props["ActiveState"]
	s.Sub = props["SubState"]
	s.Since = props["ActiveEnterTimestamp"]
	s.Enabled = props["UnitFileState"] == "enabled"
	if port, ok := knownPorts[name]; ok {
		s.Bind = resolveBindAddress(port)
	}
	return s
}

// resolveBindAddress reads the actual listening address for a port from ss output.
func resolveBindAddress(port string) string {
	out, err := exec.Command("ss", "-tlnp").Output()
	if err != nil {
		return ":" + port
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(line, ":"+port+" ") || strings.HasSuffix(fields[len(fields)-1], ":"+port) {
			// extract Local Address:Port column (column index 3)
			if len(fields) >= 4 {
				addr := fields[3]
				// normalise 0.0.0.0 / * / [::] representations
				addr = strings.Replace(addr, "*:"+port, "0.0.0.0:"+port, 1)
				addr = strings.Replace(addr, "[::]:"+port, "0.0.0.0:"+port, 1)
				if strings.HasSuffix(addr, ":"+port) {
					return addr
				}
			}
		}
	}
	return ":" + port
}

func getVPNStatus() VPNStatus {
	v := VPNStatus{Interface: "wg0-vpntorrent"}

	out, err := exec.Command("sudo", "ip", "netns", "exec", "vpntorrent",
		"wg", "show", "wg0-vpntorrent").Output()
	if err != nil {
		return v
	}
	text := string(out)
	v.Connected = strings.Contains(text, "latest handshake")

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "endpoint:"):
			v.Endpoint = strings.TrimSpace(strings.TrimPrefix(line, "endpoint:"))
		case strings.HasPrefix(line, "peer:"):
			v.PeerKey = strings.TrimSpace(strings.TrimPrefix(line, "peer:"))
		case strings.HasPrefix(line, "latest handshake:"):
			v.HandshakeAge = strings.TrimSpace(strings.TrimPrefix(line, "latest handshake:"))
		case strings.HasPrefix(line, "transfer:"):
			parts := strings.Fields(strings.TrimPrefix(line, "transfer:"))
			if len(parts) >= 6 {
				v.RxBytes = parseBytes(parts[0], parts[1])
				v.TxBytes = parseBytes(parts[3], parts[4])
			}
		}
	}
	return v
}

func journalLines(svc string, n int) []string {
	out, err := exec.Command("journalctl", "-u", svc,
		fmt.Sprintf("-n%d", n), "--no-pager", "--output=short").Output()
	if err != nil {
		return []string{"(no logs)"}
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func sudoSystemctl(action, svc string) (string, error) {
	out, err := exec.Command("sudo", "systemctl", action, svc).CombinedOutput()
	return string(out), err
}

func parseProps(s string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		if idx := strings.Index(line, "="); idx != -1 {
			m[line[:idx]] = line[idx+1:]
		}
	}
	return m
}

func parseBytes(val, unit string) int64 {
	n, _ := strconv.ParseFloat(val, 64)
	switch strings.ToUpper(strings.TrimSuffix(unit, ",")) {
	case "KIB":
		n *= 1024
	case "MIB":
		n *= 1024 * 1024
	case "GIB":
		n *= 1024 * 1024 * 1024
	}
	return int64(n)
}

// LeakCheckHandler performs an active IPv4/IPv6 leak check and returns results.
func LeakCheckHandler(w http.ResponseWriter, r *http.Request) {
	type result struct {
		HostIPv4        string `json:"host_ipv4"`
		VPNIPv4         string `json:"vpn_ipv4"`         // empty if curl not in sudoers
		Leaked          bool   `json:"leaked"`
		VethHasIPv6     bool   `json:"veth_has_ipv6"`
		IP6TablesOK     bool   `json:"ip6tables_ok"`     // false if nftables backend + no sudo
		IP6TablesNote   string `json:"ip6tables_note,omitempty"`
		WGConnected     bool   `json:"wg_connected"`
		WGHandshake     string `json:"wg_handshake"`
		WGEndpoint      string `json:"wg_endpoint"`
		WGRxBytes       int64  `json:"wg_rx_bytes"`
		WGTxBytes       int64  `json:"wg_tx_bytes"`
		KillSwitchOK    bool   `json:"kill_switch_ok"`
		CheckedAt       int64  `json:"checked_at"`
	}
	res := result{CheckedAt: time.Now().Unix()}

	// WireGuard status — reuse existing VPN status logic
	vpn := getVPNStatus()
	res.WGConnected = vpn.Connected
	res.WGHandshake = vpn.HandshakeAge
	res.WGEndpoint = vpn.Endpoint
	res.WGRxBytes = vpn.RxBytes
	res.WGTxBytes = vpn.TxBytes

	// Kill switch: default route must be via wg0-vpntorrent only
	res.KillSwitchOK = leakCheckKillSwitch()

	// Host public IPv4
	if out, err := exec.CommandContext(r.Context(), "curl", "-4", "-s", "--max-time", "6",
		"https://api.ipify.org?format=text").Output(); err == nil {
		res.HostIPv4 = strings.TrimSpace(string(out))
	}

	// VPN namespace public IPv4 — needs curl allowed in sudoers for netns
	res.VPNIPv4 = leakCheckVPNIPv4(r)
	if res.HostIPv4 != "" && res.VPNIPv4 != "" {
		res.Leaked = res.HostIPv4 == res.VPNIPv4
	}

	// IPv6 on veth-vpn — readable from /proc without root
	res.VethHasIPv6 = leakCheckVethIPv6()

	// ip6tables OUTPUT policy DROP — try sudo, fall back to /proc hint
	res.IP6TablesOK = leakCheckIP6Tables()
	if !res.IP6TablesOK {
		res.IP6TablesNote = "nftables backend — cannot verify via /proc. Run manually: sudo ip netns exec vpntorrent ip6tables -L OUTPUT -n"
	}

	jsonOK(w, res)
}

func leakCheckVPNIPv4(r *http.Request) string {
	// Resolve ip-api.com on the host side (netns has no DNS)
	addrs, err := net.LookupHost("ip-api.com")
	if err != nil || len(addrs) == 0 {
		return ""
	}
	var ipv4addr string
	for _, a := range addrs {
		if !strings.Contains(a, ":") {
			ipv4addr = a
			break
		}
	}
	if ipv4addr == "" {
		return ""
	}
	out, err := exec.CommandContext(r.Context(),
		"sudo", "ip", "netns", "exec", "vpntorrent",
		"/usr/bin/curl", "-4", "-s", "--max-time", "8",
		"--resolve", fmt.Sprintf("ip-api.com:80:%s", ipv4addr),
		"http://ip-api.com/json?fields=query",
	).Output()
	if err != nil {
		return ""
	}
	var resp struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return ""
	}
	return resp.Query
}

func leakCheckVethIPv6() bool {
	// Find TorrServer PID from systemd
	out, err := exec.Command("systemctl", "show", "torrserver-netns", "--property=MainPID").Output()
	if err != nil {
		return false
	}
	pidStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "MainPID="))
	if pidStr == "" || pidStr == "0" {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%s/net/if_inet6", pidStr))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "veth-vpn")
}

func torrsvrPID() string {
	out, err := exec.Command("systemctl", "show", "torrserver-netns", "--property=MainPID").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "MainPID="))
}

func leakCheckKillSwitch() bool {
	// Read IPv4 routing table from TorrServer's namespace via /proc — no sudo needed.
	// /proc/{pid}/net/route columns: Iface Dest Gateway Flags ... Mask ...
	// Default route: Dest=00000000 Mask=00000000.  Kill switch: Iface must be wg0-vpntorrent.
	pid := torrsvrPID()
	if pid == "" || pid == "0" {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%s/net/route", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		// Dest=00000000 AND Mask=00000000 → default route
		if fields[1] == "00000000" && fields[7] == "00000000" {
			return fields[0] == "wg0-vpntorrent"
		}
	}
	return false
}

func leakCheckIP6Tables() bool {
	// Try sudo first (works if ip6tables is in sudoers)
	out, err := exec.Command("sudo", "ip", "netns", "exec", "vpntorrent",
		"ip6tables", "-L", "OUTPUT", "-n").Output()
	if err == nil {
		return strings.Contains(string(out), "policy DROP")
	}
	// Fall back: /proc/{pid}/net/ip6_tables_names contains table names if rules exist.
	// The presence of the "filter" table means ip6tables rules have been loaded.
	pid := torrsvrPID()
	if pid == "" || pid == "0" {
		return false
	}
	data, err2 := os.ReadFile(fmt.Sprintf("/proc/%s/net/ip6_tables_names", pid))
	if err2 != nil {
		return false
	}
	return strings.Contains(string(data), "filter")
}

// ── User management ───────────────────────────────────────────────────────────

type userDTO struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Source   string `json:"source"`
	IsAdmin  bool   `json:"is_admin"`
	Created  string `json:"created_at"`
}

func toUserDTO(u *store.User) userDTO {
	return userDTO{u.ID, u.Username, u.AuthSource, u.IsAdmin, u.CreatedAt.Format(time.RFC3339)}
}

// UsersHandler — GET /api/users — list all orchestrator users.
func UsersHandler(w http.ResponseWriter, r *http.Request) {
	users, err := db.ListUsers()
	if err != nil {
		jsonErr(w, "db error", http.StatusInternalServerError)
		return
	}
	out := make([]userDTO, len(users))
	for i, u := range users {
		out[i] = toUserDTO(u)
	}
	jsonOK(w, out)
}

// CreateUserHandler — POST /api/users — create a new local user.
func CreateUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 8 {
		jsonErr(w, "username required and password must be ≥8 chars", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonErr(w, "error hashing password", http.StatusInternalServerError)
		return
	}
	u := &store.User{
		Username:     req.Username,
		PasswordHash: string(hash),
		AuthSource:   "local",
		IsAdmin:      req.IsAdmin,
	}
	if err := db.CreateUser(u); err != nil {
		jsonErr(w, "username already exists", http.StatusConflict)
		return
	}
	created, _ := db.GetUserByUsername(req.Username)
	jsonOK(w, toUserDTO(created))
}

// JellyfinUsersHandler — GET /api/jellyfin/users — lists Jellyfin users for the import picker.
func JellyfinUsersHandler(jfc *jellyfin.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := jfc.ListUsers()
		if err != nil {
			jsonErr(w, "failed to list Jellyfin users: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Filter out users already imported
		existing, _ := db.ListUsers()
		imported := map[string]bool{}
		for _, u := range existing {
			if u.JellyfinUserID != "" {
				imported[u.JellyfinUserID] = true
			}
		}
		type jfUserOut struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Imported bool   `json:"imported"`
		}
		out := make([]jfUserOut, len(users))
		for i, u := range users {
			out[i] = jfUserOut{u.ID, u.Name, imported[u.ID]}
		}
		jsonOK(w, out)
	}
}

// ImportUserHandler — POST /api/users/import — import a Jellyfin user.
func ImportUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		JellyfinUserID string `json:"jellyfin_user_id"`
		Username       string `json:"username"`
		IsAdmin        bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.JellyfinUserID == "" || req.Username == "" {
		jsonErr(w, "jellyfin_user_id and username required", http.StatusBadRequest)
		return
	}
	u := &store.User{
		Username:       strings.TrimSpace(req.Username),
		JellyfinUserID: req.JellyfinUserID,
		AuthSource:     "jellyfin",
		PasswordHash:   "",
		IsAdmin:        req.IsAdmin,
	}
	if err := db.CreateUser(u); err != nil {
		jsonErr(w, "username already exists", http.StatusConflict)
		return
	}
	created, _ := db.GetUserByUsername(u.Username)
	jsonOK(w, toUserDTO(created))
}

// UpdateUserHandler — PATCH /api/users/{id} — update is_admin or username.
func UpdateUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, "bad id", http.StatusBadRequest)
		return
	}
	caller := UserFromContext(r)
	if caller != nil && caller.ID == id {
		jsonErr(w, "cannot modify your own account via this endpoint", http.StatusBadRequest)
		return
	}
	target, err := db.GetUserByID(id)
	if err != nil || target == nil {
		jsonErr(w, "user not found", http.StatusNotFound)
		return
	}
	var req struct {
		IsAdmin  *bool   `json:"is_admin"`
		Username *string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.IsAdmin != nil {
		target.IsAdmin = *req.IsAdmin
	}
	if req.Username != nil && strings.TrimSpace(*req.Username) != "" {
		target.Username = strings.TrimSpace(*req.Username)
	}
	if err := db.UpdateUser(target); err != nil {
		jsonErr(w, "update failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toUserDTO(target))
}

// DeleteUserHandler — DELETE /api/users/{id} — delete a user.
func DeleteUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, "bad id", http.StatusBadRequest)
		return
	}
	caller := UserFromContext(r)
	if caller != nil && caller.ID == id {
		jsonErr(w, "cannot delete your own account", http.StatusBadRequest)
		return
	}
	if err := db.DeleteUser(id); err != nil {
		jsonErr(w, "delete failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "deleted"})
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
