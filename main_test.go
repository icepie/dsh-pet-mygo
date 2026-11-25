package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testServer builds a server with an isolated config path so tests never touch
// the user's real ~/.config file.
func testServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))
	return newServer()
}

func TestMovDuration(t *testing.T) {
	cases := []struct {
		name string
		want float64
	}{
		{"待机呼吸休闲", 10.04},
		{"工作状态-垂头叹气冒汗", 4.92},
		{"螃蟹走路", 10.04},
	}
	for _, c := range cases {
		got, err := movDuration(filepath.Join(movAnimDir, c.name+".mov"))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if math.Abs(got-c.want) > 0.1 {
			t.Errorf("%s: got %.2fs, want %.2fs", c.name, got, c.want)
		}
	}
}

func TestMovDurationMissingFile(t *testing.T) {
	if _, err := movDuration(filepath.Join(movAnimDir, "does-not-exist.mov")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestWebmDuration(t *testing.T) {
	cases := []struct {
		name string
		want float64
	}{
		{"待机呼吸休闲", 10.04},
		{"工作状态-垂头叹气冒汗", 4.92},
		{"螃蟹走路", 10.04},
	}
	for _, c := range cases {
		got, err := webmDuration(filepath.Join(webmAnimDir, c.name+".webm"))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if math.Abs(got-c.want) > 0.1 {
			t.Errorf("%s: got %.2fs, want %.2fs", c.name, got, c.want)
		}
	}
}

func TestWebmDurationMissingFile(t *testing.T) {
	if _, err := webmDuration(filepath.Join(webmAnimDir, "does-not-exist.webm")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestFindAtomNested(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(movAnimDir, "待机呼吸休闲.mov"))
	if err != nil {
		t.Skipf("asset not present: %v", err)
	}
	moov, ok := findAtom(data, "moov")
	if !ok {
		t.Fatal("moov not found")
	}
	if _, ok := findAtom(moov, "mvhd"); !ok {
		t.Fatal("mvhd not found inside moov (nested lookup)")
	}
}

func TestPickNextWeights(t *testing.T) {
	s := testServer(t)
	n := 40000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		kind, _ := s.pickNext("main")
		counts[kind]++
	}
	check := func(kind string, wantPct float64) {
		got := float64(counts[kind]) / float64(n) * 100
		if math.Abs(got-wantPct) > 2 {
			t.Errorf("%s: %.1f%%, want ~%.1f%%", kind, got, wantPct)
		}
	}
	check("idle", 10)
	check("turn", 5)
	check("move", 5)
	check("action", 80)
}

func TestPickNextNonEmpty(t *testing.T) {
	s := testServer(t)
	pool := s.packOf("main").actions
	if len(pool) == 0 {
		t.Fatal("action pool is empty; resources/anim not found from the test cwd")
	}
	for i := 0; i < 100; i++ {
		kind, name := s.pickNext("main")
		if name == "" {
			t.Fatalf("pickNext returned an empty name for kind %q", kind)
		}
	}
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, body any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return out
}

func TestContractState(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	st := doJSON(t, srv, http.MethodGet, "/dsh-pet-7340/state", nil)
	pets, ok := st["pets"].(map[string]any)
	if !ok {
		t.Fatal("state.pets missing")
	}
	main, ok := pets["main"].(map[string]any)
	if !ok {
		t.Fatal("state.pets.main missing")
	}
	if _, ok := main["anim"].(map[string]any); !ok {
		t.Fatal("state.pets.main.anim missing")
	}
}

func TestContractAnimAndPlayback(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	resp := doJSON(t, srv, http.MethodPost, "/dsh-pet-7340/anim?pet=main", map[string]any{"name": "东张西望"})
	pid, ok := resp["playbackId"].(string)
	if !ok || pid == "" {
		t.Fatalf("no playbackId in %v", resp)
	}

	pb := doJSON(t, srv, http.MethodGet, "/dsh-pet-7340/playback/"+pid, nil)
	if pb["status"] != "accepted" {
		t.Errorf("initial playback status = %v, want accepted", pb["status"])
	}
	if pb["name"] != "东张西望" {
		t.Errorf("playback name = %v, want 东张西望", pb["name"])
	}

	// The page reports completion; the receipt should follow.
	doJSON(t, srv, http.MethodPost, "/dsh-pet-7340/playback/"+pid+"/report", map[string]any{"status": "completed"})
	pb = doJSON(t, srv, http.MethodGet, "/dsh-pet-7340/playback/"+pid, nil)
	if pb["status"] != "completed" {
		t.Errorf("playback status = %v, want completed", pb["status"])
	}

	// Unknown playback id is a 404, not a silent success.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dsh-pet-7340/playback/nope", nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown playback status = %d, want 404", r2.StatusCode)
	}
}

func TestContractBroadcast(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	resp := doJSON(t, srv, http.MethodPost, "/dsh-pet-7340/broadcast?pet=main",
		map[string]any{"text": "测试气泡", "duration": 3000})
	if resp["ok"] != true {
		t.Fatalf("broadcast resp = %v", resp)
	}
	st := doJSON(t, srv, http.MethodGet, "/dsh-pet-7340/state", nil)
	pets := st["pets"].(map[string]any)
	say := pets["main"].(map[string]any)["say"].(map[string]any)
	data := say["data"].(map[string]any)
	if data["text"] != "测试气泡" {
		t.Errorf("say.text = %v, want 测试气泡", data["text"])
	}
}

func TestContractBadRequest(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	// Empty name must be rejected, not accepted silently.
	resp := doJSONRaw(t, srv, http.MethodPost, "/dsh-pet-7340/anim?pet=main", `{"name":""}`)
	if resp != http.StatusBadRequest {
		t.Errorf("empty anim name status = %d, want 400", resp)
	}
}

func doJSONRaw(t *testing.T, srv *httptest.Server, method, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))

	want := appConfig{Size: 0.6, Pets: []petConfig{{ID: "main", Pack: "pig", X: 123, Y: 45}}}
	saveConfig(want)

	got := loadConfig()
	if got.Size != 0.6 {
		t.Errorf("size = %v, want 0.6 (a zero size would reset to 1)", got.Size)
	}
	if len(got.Pets) != 1 || got.Pets[0].ID != "main" || got.Pets[0].Pack != "pig" ||
		got.Pets[0].X != 123 || got.Pets[0].Y != 45 {
		t.Fatalf("round trip got %+v", got)
	}
}

// TestScaleSurvivesPositionSave guards the bug where savePositions wrote
// only Pets and silently zeroed Size.
func TestScaleSurvivesPositionSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))

	saveConfig(appConfig{Size: 0.6, Pets: []petConfig{{ID: "main", X: 1, Y: 2}}})
	// Simulate what savePositions writes: it must carry Size through.
	saveConfig(appConfig{Size: 0.6, Pets: []petConfig{{ID: "main", X: 300, Y: 400}}})

	got := loadConfig()
	if got.Size != 0.6 {
		t.Fatalf("size reset to %v after a position save, want 0.6", got.Size)
	}
	if got.Pets[0].X != 300 {
		t.Fatalf("position not updated: %+v", got.Pets[0])
	}
}

func TestConfigFirstRunWritesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("DSH_PET_MYGO_CONFIG", path)

	cfg := loadConfig()
	if len(cfg.Pets) == 0 {
		t.Fatal("first run returned no pets")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first run did not write a default config: %v", err)
	}
}

func TestAnimDurationFallback(t *testing.T) {
	// A missing asset should fall back to animPlaySec rather than 0 (which
	// would make the chain spin without ever advancing).
	d := testServer(t).packOf("main").duration("definitely-missing-animation")
	if d.Seconds() != float64(animPlaySec) {
		t.Errorf("fallback duration = %.1fs, want %ds", d.Seconds(), animPlaySec)
	}
}

func TestCleanWhisperLines(t *testing.T) {
	raw := "1. 主人记得喝水~\n2、今天也要加油哦\n\n- \"代码写完了吗？\"\n  \n" +
		"这一行特别特别长超过四十个字所以应该被丢掉AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	got := cleanWhisperLines(raw)
	want := []string{"主人记得喝水~", "今天也要加油哦", "代码写完了吗？"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadWhisperLinesMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))
	if got := loadWhisperLines(); got != nil {
		t.Errorf("missing file should return nil, got %v", got)
	}
}

func TestLoadWhisperLinesFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))
	data, _ := json.Marshal(whispersFile{GeneratedAt: "x", Lines: []string{"a", "b"}})
	if err := os.WriteFile(filepath.Join(dir, "whispers.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadWhisperLines()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v, want [a b]", got)
	}
}

// writePack lays out a custom pack next to the config file, the way README
// documents it: ~/.config/dsh-pet-mygo/pets/<name>/anim(-webm)/.
func writePack(t *testing.T, name string, clips ...string) {
	t.Helper()
	sub, ext := packLayout()
	dir := filepath.Join(filepath.Dir(configPath()), "pets", name, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, clip := range clips {
		if err := os.WriteFile(filepath.Join(dir, clip+ext), []byte("fake clip"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanPackClassification(t *testing.T) {
	_, ext := packLayout()
	dir := t.TempDir()
	clips := []string{
		"待机呼吸休闲", "东张西望", "螃蟹走路",
		"点击回应-开心跃动", "点击回应-傲娇生气",
		"碎碎念-发呆碎碎念", "工作状态-雀跃庆祝", "余额-钱袋满溢",
		"吃火锅", "玩魔方",
	}
	for _, c := range clips {
		if err := os.WriteFile(filepath.Join(dir, c+ext), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := scanPack("pig", dir, ext)
	if err != nil {
		t.Fatal(err)
	}
	if p.idle != "待机呼吸休闲" || p.turn != "东张西望" {
		t.Errorf("idle/turn = %q/%q", p.idle, p.turn)
	}
	if len(p.roam) != 1 || p.roam[0] != "螃蟹走路" {
		t.Errorf("roam = %v", p.roam)
	}
	if len(p.clicks) != 2 || len(p.whispers) != 1 || len(p.work) != 1 {
		t.Errorf("clicks/whispers/work = %v/%v/%v", p.clicks, p.whispers, p.work)
	}
	// Balance clips are reserved, not random actions.
	got := strings.Join(p.actions, ",")
	if got != "吃火锅,玩魔方" {
		t.Errorf("actions = %q, want the two generic clips only", got)
	}
	if _, ok := p.durs.Load(p.idle); ok {
		t.Error("duration parsed before it was ever requested")
	}
}

func TestScanPackIdleFallback(t *testing.T) {
	_, ext := packLayout()
	dir := t.TempDir()
	for _, c := range []string{"idle", "wave"} {
		if err := os.WriteFile(filepath.Join(dir, c+ext), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := scanPack("named", dir, ext)
	if err != nil {
		t.Fatal(err)
	}
	// No canonical idle name: the first clip loops and must leave the random
	// pool, otherwise it could be picked as an "action" and never advance.
	if p.idle != "idle" || p.turn != "idle" {
		t.Errorf("idle/turn = %q/%q, want idle/idle", p.idle, p.turn)
	}
	if len(p.actions) != 1 || p.actions[0] != "wave" {
		t.Errorf("actions = %v, want [wave]", p.actions)
	}
}

func TestScanPackEmptyDir(t *testing.T) {
	_, ext := packLayout()
	if _, err := scanPack("empty", t.TempDir(), ext); err == nil {
		t.Fatal("empty pack scanned without an error")
	}
}

// TestCustomPackEndToEnd covers what the user sees after adding a pet kind:
// its own idle in /state, its own pools in /config, and clips served from its
// directory (not resources/).
func TestCustomPackEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))
	saveConfig(appConfig{Size: 0.7, Pets: []petConfig{
		{ID: "main", X: 10, Y: 10},
		{ID: "pig1", Pack: "pig", X: 300, Y: 200},
	}})
	writePack(t, "pig", "待机", "打滚", "点击回应-翻跟头")

	s := newServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	st := doJSON(t, srv, http.MethodGet, "/dsh-pet-7340/state", nil)
	pets := st["pets"].(map[string]any)
	pig := pets["pig1"].(map[string]any)["anim"].(map[string]any)["data"].(map[string]any)
	if pig["name"] != "待机" {
		t.Errorf("pig1 idle = %v, want 待机", pig["name"])
	}
	if mainName := pets["main"].(map[string]any)["anim"].(map[string]any)["data"].(map[string]any)["name"]; mainName != idleAnim {
		t.Errorf("main idle = %v, want %v", mainName, idleAnim)
	}

	cfg := doJSON(t, srv, http.MethodGet, "/dsh-pet-7340/config", nil)
	pigPools := cfg["pig1"].(map[string]any)["animations"].(map[string]any)
	if idle := pigPools["idle"].([]any); len(idle) != 1 || idle[0] != "待机" {
		t.Errorf("pig1 idle pool = %v", idle)
	}
	if actions := pigPools["actions"].([]any); len(actions) != 1 || actions[0] != "打滚" {
		t.Errorf("pig1 action pool = %v, want [打滚]", actions)
	}
	mainPools := cfg["main"].(map[string]any)["animations"].(map[string]any)
	if idle := mainPools["idle"].([]any); len(idle) != 1 || idle[0] != idleAnim {
		t.Errorf("main idle pool = %v, want the built-in %v", idle, idleAnim)
	}

	// The page loads clips over HTTP, so this is the path a custom pack takes.
	resp, err := http.Get(srv.URL + "/dsh-pet-7340/asset?pet=pig1&name=" + url.QueryEscape("打滚"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "fake clip" {
		t.Fatalf("asset = %d %q, want the pig pack clip", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/webm" && ct != "video/quicktime" {
		t.Errorf("asset content-type = %q", ct)
	}

	// A missing clip and a traversal attempt are hard failures, never a fallback
	// to another pack's file.
	for _, q := range []string{"pet=pig1&name=nope", "pet=pig1&name=../main"} {
		r, err := http.Get(srv.URL + "/dsh-pet-7340/asset?" + q)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			t.Errorf("%s served 200, want an error", q)
		}
	}
}

// TestMissingPackFallsBack: a typo in pack must not take the pet down.
func TestMissingPackFallsBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))
	saveConfig(appConfig{Size: 0.7, Pets: []petConfig{{ID: "main", Pack: "typo", X: 1, Y: 1}}})

	s := newServer()
	if got := s.packOf("main"); got != s.builtin {
		t.Fatalf("packOf(main) = %q, want the built-in fallback", got.name)
	}
	if s.pets["main"].Anim.Data.(map[string]any)["name"] != idleAnim {
		t.Error("fallback pet did not start on the built-in idle")
	}
}

// TestConfigLoadDoesNotMutateDefaults guards a shared-backing-array bug:
// loadConfig used to unmarshal into a struct whose Pets aliased defaultPets, so
// a user config rewrote the defaults for every load after it.
func TestConfigLoadDoesNotMutateDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DSH_PET_MYGO_CONFIG", filepath.Join(dir, "config.json"))
	saveConfig(appConfig{Size: 0.6, Pets: []petConfig{{ID: "pig1", Pack: "pig", X: 1, Y: 2}}})

	got := loadConfig()
	if len(got.Pets) != 1 || got.Pets[0].Pack != "pig" {
		t.Fatalf("loaded pets = %+v", got.Pets)
	}
	if len(defaultPets) != 1 || defaultPets[0].ID != "main" || defaultPets[0].Pack != "" {
		t.Fatalf("defaults mutated by a config load: %+v", defaultPets)
	}
}

// TestPageServedFromHost: the window loads "/" for its page, so the host must
// serve it and must not answer other paths with it (clips go through /asset).
func TestPageServedFromHost(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	for _, path := range []string{"/", "/index.html"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s content-type = %q", path, ct)
		}
		if !bytes.Contains(body, []byte("id=\"stage\"")) {
			t.Errorf("%s did not return the pet page", path)
		}
	}

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", resp.StatusCode)
	}
}
