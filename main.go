package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

const (
	appName = "tailgate"
	uiAddr  = "127.0.0.1:9473"
)

// Forward exposes Host:Port (on the tailnet) on this machine's LAN at LocalPort.
type Forward struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	LocalPort int    `json:"localPort"` // defaults to Port
}

type Config struct {
	AuthKey  string    `json:"authKey,omitempty"`
	Running  bool      `json:"running"`
	Forwards []Forward `json:"forwards"`
}

type App struct {
	mu      sync.Mutex
	cfg     Config
	cfgPath string
	dir     string
	srv     *tsnet.Server
	lns     []net.Listener
	err     string
	up      bool
}

func main() {
	install := flag.Bool("install", false, "install launch-at-login entry and exit")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser on start")
	flag.Parse()

	if *install {
		if err := installAutostart(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Installed. It will start at next login.")
		return
	}

	base, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Join(base, appName)
	os.MkdirAll(dir, 0o700)

	a := &App{dir: dir, cfgPath: filepath.Join(dir, "config.json")}
	if b, err := os.ReadFile(a.cfgPath); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if a.validForwards() == nil && a.cfg.Running {
		go a.start()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.page)
	mux.HandleFunc("/status", a.status)
	mux.HandleFunc("/start", a.handleStart)
	mux.HandleFunc("/stop", a.handleStop)

	ln, err := net.Listen("tcp", uiAddr)
	if err != nil {
		log.Fatalf("UI port in use (is it already running?): %v", err)
	}
	if !*noBrowser {
		go func() { time.Sleep(300 * time.Millisecond); openBrowser("http://" + uiAddr) }()
	}
	log.Printf("UI at http://%s  (config: %s)", uiAddr, a.cfgPath)
	log.Fatal(http.Serve(ln, mux))
}

func (a *App) validForwards() error { return validate(a.cfg.Forwards) }

func validate(fs []Forward) error {
	if len(fs) == 0 {
		return fmt.Errorf("add at least one host")
	}
	seen := map[int]bool{}
	for _, f := range fs {
		if f.Host == "" || f.Port == 0 {
			return fmt.Errorf("every row needs a host and port")
		}
		if seen[f.LocalPort] {
			return fmt.Errorf("local port %d is used twice; set a different local port", f.LocalPort)
		}
		seen[f.LocalPort] = true
	}
	return nil
}

func (a *App) save() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	os.WriteFile(a.cfgPath, b, 0o600)
}

func (a *App) start() {
	a.mu.Lock()
	if a.srv != nil {
		a.mu.Unlock()
		return
	}
	a.err = ""
	a.up = false
	s := &tsnet.Server{
		Hostname: appName,
		Dir:      filepath.Join(a.dir, "ts"),
		AuthKey:  a.cfg.AuthKey,
		Logf:     func(string, ...any) {},
	}
	a.srv = s
	fwds := append([]Forward(nil), a.cfg.Forwards...)
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Up(ctx); err != nil {
		a.fail("Tailscale connect failed: " + err.Error())
		return
	}

	var lns []net.Listener
	for _, f := range fwds {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", f.LocalPort))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			a.fail(fmt.Sprintf("can't listen on port %d: %v", f.LocalPort, err))
			return
		}
		lns = append(lns, ln)
	}

	a.mu.Lock()
	a.lns = lns
	a.up = true
	a.cfg.AuthKey = "" // no longer needed once the node state is saved
	a.cfg.Running = true
	a.save()
	a.mu.Unlock()

	for i, f := range fwds {
		go serve(s, lns[i], net.JoinHostPort(f.Host, fmt.Sprint(f.Port)))
	}
}

func serve(s *tsnet.Server, ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			dctx, dc := context.WithTimeout(context.Background(), 15*time.Second)
			defer dc()
			up, err := s.Dial(dctx, "tcp", target)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); done <- struct{}{} }()
			go func() { io.Copy(c, up); done <- struct{}{} }()
			<-done
		}()
	}
}

func (a *App) fail(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = msg
	if a.srv != nil {
		a.srv.Close()
		a.srv = nil
	}
}

func (a *App) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.lns {
		l.Close()
	}
	a.lns = nil
	if a.srv != nil {
		a.srv.Close()
		a.srv = nil
	}
	a.up = false
	a.cfg.Running = false
	a.save()
}

func (a *App) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var in struct {
		AuthKey  string    `json:"authKey"`
		Forwards []Forward `json:"forwards"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for i := range in.Forwards {
		f := &in.Forwards[i]
		f.Host = strings.TrimSpace(f.Host)
		if f.LocalPort == 0 {
			f.LocalPort = f.Port
		}
	}
	if err := validate(in.Forwards); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a.mu.Lock()
	a.cfg.Forwards = in.Forwards
	if k := strings.TrimSpace(in.AuthKey); k != "" {
		a.cfg.AuthKey = k
	}
	a.save()
	a.mu.Unlock()
	go a.start()
	w.WriteHeader(204)
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	a.stop()
	w.WriteHeader(204)
}

type fwdStatus struct {
	Forward
	Path string   `json:"path"`
	LAN  []string `json:"lan"`
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	state := "stopped"
	if a.srv != nil {
		state = "connecting"
	}
	if a.up {
		state = "running"
	}
	srv := a.srv
	fwds := append([]Forward(nil), a.cfg.Forwards...)
	errMsg := a.err
	a.mu.Unlock()

	out := make([]fwdStatus, 0, len(fwds))
	for _, f := range fwds {
		fs := fwdStatus{Forward: f, LAN: lanAddrs(f.LocalPort)}
		if srv != nil && state == "running" {
			fs.Path = pathTo(srv, f.Host)
		}
		out = append(out, fs)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"state":    state,
		"error":    errMsg,
		"forwards": out,
	})
}

// pathTo reports "direct", "relay" or "unknown" for the peer matching host.
func pathTo(s *tsnet.Server, host string) string {
	lc, err := s.LocalClient()
	if err != nil {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := lc.Status(ctx)
	if err != nil {
		return "unknown"
	}
	t := strings.ToLower(host)
	for _, p := range st.Peer {
		match := strings.HasPrefix(strings.ToLower(p.HostName), strings.Split(t, ".")[0])
		for _, ip := range p.TailscaleIPs {
			if ip.String() == t {
				match = true
			}
		}
		if match {
			if p.CurAddr != "" {
				return "direct"
			}
			if p.Relay != "" {
				return "relay"
			}
		}
	}
	return "unknown"
}

func lanAddrs(port int) []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		ipn, ok := ad.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		ip := ipn.IP.To4()
		if ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 { // skip tailscale CGNAT
			continue
		}
		if ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", ip, port))
	}
	return out
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func installAutostart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		p := filepath.Join(home, "Library/LaunchAgents/com."+appName+".plist")
		os.MkdirAll(filepath.Dir(p), 0o755)
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>--no-browser</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
</dict></plist>`, appName, exe)
		if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
			return err
		}
		return exec.Command("launchctl", "load", p).Run()
	case "windows":
		return exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
			"/v", appName, "/t", "REG_SZ", "/d", `"`+exe+`" --no-browser`, "/f").Run()
	default:
		p := filepath.Join(home, ".config/systemd/user/"+appName+".service")
		os.MkdirAll(filepath.Dir(p), 0o755)
		unit := fmt.Sprintf(`[Unit]
Description=Tailgate: LAN to tailnet port forwarder
After=network-online.target

[Service]
ExecStart=%s --no-browser
Restart=always

[Install]
WantedBy=default.target
`, exe)
		if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
			return err
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return exec.Command("systemctl", "--user", "enable", "--now", appName+".service").Run()
	}
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHTML)
}

const pageHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Tailgate</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root{color-scheme:light dark;font-family:system-ui,sans-serif}
body{max-width:600px;margin:40px auto;padding:0 16px}
label{display:block;margin:12px 0 4px;font-size:.9em;opacity:.8}
input{box-sizing:border-box;padding:8px;font-size:1em}
button{margin-top:16px;padding:10px 18px;font-size:1em;cursor:pointer}
.dot{display:inline-block;width:12px;height:12px;border-radius:50%;margin-right:8px;background:#888}
.running .dot{background:#2ecc71}.connecting .dot{background:#f1c40f}
.err{color:#e74c3c;margin-top:12px}
.addr{font-family:monospace;font-size:1.1em;margin:2px 0}
.row{display:flex;gap:6px;margin:6px 0}
.row input{min-width:0}
.h{flex:3}.p{flex:1}
.x{margin:0;padding:4px 10px}
h3{margin:18px 0 4px}
.hdr{display:flex;gap:6px;font-size:.8em;opacity:.7}
</style></head><body>
<h2><span id="s" class="stopped"><span class="dot"></span><span id="st">Stopped</span></span></h2>
<div id="err" class="err"></div>
<div id="info"></div>
<div id="form">
<label>Auth key (first run only)</label><input id="key" type="password" style="width:100%" placeholder="tskey-auth-...">
<label>Hosts to expose</label>
<div class="hdr"><span class="h">Tailnet host</span><span class="p">Port</span><span class="p">Local port</span><span style="width:34px"></span></div>
<div id="rows"></div>
<button id="add" type="button">+ Add host</button>
</div>
<button id="btn"></button>
<script>
let state="stopped",filled=false;
function addRow(f){
  const d=document.createElement('div');d.className='row';
  d.innerHTML='<input class="h" placeholder="media-server"><input class="p" type="number" placeholder="8096"><input class="p" type="number" placeholder="same">'
    +'<button class="x" type="button">x</button>';
  const i=d.querySelectorAll('input');
  i[0].value=f.host||'';i[1].value=f.port||'';i[2].value=(f.localPort&&f.localPort!=f.port)?f.localPort:'';
  d.querySelector('.x').onclick=()=>d.remove();
  document.getElementById('rows').appendChild(d);
}
function collect(){
  return [...document.querySelectorAll('#rows .row')].map(d=>{
    const i=d.querySelectorAll('input');
    return {host:i[0].value,port:parseInt(i[1].value)||0,localPort:parseInt(i[2].value)||0};
  });
}
async function poll(){
  const j=await (await fetch('/status')).json();
  state=j.state;
  document.getElementById('s').className=state;
  document.getElementById('st').textContent=state=='running'?'Connected':state=='connecting'?'Connecting...':'Stopped';
  document.getElementById('err').textContent=j.error||'';
  if(!filled){(j.forwards.length?j.forwards:[{}]).forEach(addRow);filled=true}
  document.getElementById('form').style.display=state=='stopped'?'block':'none';
  document.getElementById('btn').textContent=state=='stopped'?'Start':'Stop';
  document.getElementById('info').innerHTML=state=='running'
    ?'<p>Point your device at:</p>'+j.forwards.map(f=>'<h3>'+f.host+':'+f.port+(f.path&&f.path!='unknown'?' ('+f.path+')':'')+'</h3>'
      +(f.lan.map(a=>'<div class="addr">'+a+'</div>').join('')||'<div>No LAN address found</div>')).join(''):'';
}
document.getElementById('add').onclick=()=>addRow({});
document.getElementById('btn').onclick=async()=>{
  if(state=='stopped'){
    const r=await fetch('/start',{method:'POST',body:JSON.stringify({authKey:document.getElementById('key').value,forwards:collect()})});
    if(!r.ok)document.getElementById('err').textContent=await r.text();
  }else await fetch('/stop',{method:'POST'});
  poll();
};
poll();setInterval(poll,2000);
</script></body></html>`
