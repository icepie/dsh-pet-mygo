// dsh-pet-mygo: a desktop pet that runs without DeepSeek Harness or Electron.
// A transparent, always-on-top MyGo window per pet plays HEVC-with-alpha .mov
// assets, driven through dsh-pet's own /dsh-pet-7340 HTTP contract, so the
// existing pi bridge extension (and any other consumer) keeps working unchanged.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

const (
	listenAddr  = "127.0.0.1:3080"
	idleAnim    = "待机呼吸休闲"
	baseW       = 640 // animation canvas
	baseH       = 360
	movAnimDir  = "resources/anim"      // HEVC-with-alpha .mov (WKWebView)
	webmAnimDir = "resources/anim-webm" // VP9-with-alpha .webm (WebKitGTK, WebView2)

	// petHit* is the pet body region (in canvas coords) used as the fallback
	// click-through box; everything else in the window passes clicks through.
	petHitMinX = 196
	petHitMinY = 60
	petHitMaxX = 447
	petHitMaxY = 331
)

// windowW/H and the fallback hit box are derived from the config scale at
// startup (see applyScale), so the whole canvas scales together.
var (
	// Windows' WKWebView plays HEVC-with-alpha .mov; WebKitGTK and WebView2
	// play VP9-with-alpha .webm. Pick the set the platform's webview decodes.
	animDir = movAnimDir
	animExt = ".mov"

	windowW = baseW
	windowH = baseH
	hitMinX = petHitMinX
	hitMinY = petHitMinY
	hitMaxX = petHitMaxX
	hitMaxY = petHitMaxY
)

func init() {
	if runtime.GOOS != "darwin" {
		animDir = webmAnimDir
		animExt = ".webm"
	}
}

// applyScale sizes the window (and the fallback hit box) to the canvas times
// the config scale, clamped to a sane range.
func applyScale(size float64) {
	if size < 0.3 {
		size = 0.3
	}
	if size > 2 {
		size = 2
	}
	windowW = int(float64(baseW) * size)
	windowH = int(float64(baseH) * size)
	hitMinX = int(float64(petHitMinX) * size)
	hitMinY = int(float64(petHitMinY) * size)
	hitMaxX = int(float64(petHitMaxX) * size)
	hitMaxY = int(float64(petHitMaxY) * size)
}

// workStatusAnims is the animation pool for the six work-status tiers, in the
// same order dsh-pet uses (and the same names as resources/anim/*.mov).
var workStatusAnims = []string{
	"工作状态-思考冒泡",
	"工作状态-忙碌点按",
	"工作状态-清点归档",
	"工作状态-原地踱步张望",
	"工作状态-雀跃庆祝",
	"工作状态-垂头叹气冒汗",
}

// roamAnims are the walk animations played while the pet roams across screen.
var roamAnims = []string{"螃蟹走路", "原地漂浮踏步", "原地左转奔跑"}

// clickAnims play when the pet is clicked (random pick).
var clickAnims = []string{
	"点击回应-开心跃动",
	"点击回应-害羞惊讶",
	"点击回应-傲娇生气",
	"点击回应-挠痒咯咯笑",
	"点击回应-元气挥手",
}

// whisperAnims play while the pet mutters to itself.
var whisperAnims = []string{"碎碎念-擦桌碎碎念", "碎碎念-发呆碎碎念", "碎碎念-对屏碎碎念"}

// whisperLines are the built-in fallback when no model provider is configured:
// the pet picks one at random, so it still talks without an LLM.
var whisperLines = []string{
	"主人记得喝水~",
	"今天也要加油哦~",
	"先歇一小会儿嘛~",
	"肚子有点饿了…",
	"代码写完了吗？",
	"要不要休息一下眼睛~",
	"我在旁边陪着你呢~",
	"好像快下班啦~",
	"今天天气不错的样子~",
	"数到三就起来活动下~",
}

// whispersFile is the on-disk cache produced by -generate-whispers.
type whispersFile struct {
	GeneratedAt string   `json:"generatedAt"`
	Lines       []string `json:"lines"`
}

func whisperLinesPath() string {
	return filepath.Join(filepath.Dir(configPath()), "whispers.json")
}

// loadWhisperLines reads the pre-generated line cache, or nil if absent.
func loadWhisperLines() []string {
	data, err := os.ReadFile(whisperLinesPath())
	if err != nil {
		return nil
	}
	var wf whispersFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil
	}
	return wf.Lines
}

// llmConfig resolves the chat-completions endpoint: explicit env vars first,
// then pi's local magpie proxy (available while pi is running).
func llmConfig() (base, key, model string) {
	base = os.Getenv("DSH_PET_LLM_BASE")
	key = os.Getenv("DSH_PET_LLM_KEY")
	model = os.Getenv("DSH_PET_LLM_MODEL")
	if base == "" {
		base = "http://127.0.0.1:3425/v1"
	}
	if key == "" {
		key = "magpie"
	}
	if model == "" {
		model = "group/smart-split"
	}
	return base, key, model
}

// cleanWhisperLines tidies raw model output into short one-liners.
func cleanWhisperLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "0123456789.、)） -•·")
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "\"'“”「」『』")
		if line == "" || len([]rune(line)) > 40 {
			continue
		}
		out = append(out, line)
	}
	return out
}

// generateWhispers asks the model for a batch of lines and caches them, so the
// running pet never calls the model itself (a batch costs ~1k tokens and lasts
// days). Exposed as the -generate-whispers command.
func generateWhispers(n int) error {
	base, key, model := llmConfig()
	prompt := fmt.Sprintf("你是主人桌面上的Q版蓝发小女仆。生成 %d 句碎碎念，每句一行，20字以内，"+
		"自然随意、温柔俏皮，说人话不啰嗦，不解释你自己，不提AI，不要编号不要引号。", n)
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"temperature": 0.9,
		"max_tokens":  1500,
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llm returned %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if len(out.Choices) == 0 {
		return errors.New("empty completion")
	}
	lines := cleanWhisperLines(out.Choices[0].Message.Content)
	if len(lines) == 0 {
		return errors.New("no usable lines in completion")
	}
	wf := whispersFile{GeneratedAt: time.Now().Format(time.RFC3339), Lines: lines}
	raw, _ := json.MarshalIndent(wf, "", "  ")
	path := whisperLinesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	log.Printf("wrote %d whisper lines to %s", len(lines), path)
	return nil
}

// Animation-chain weights, matching dsh-pet's config.jsonc defaults:
// idle 10 + turn 5 + move 5 + categories 80 = 100.
const (
	weightIdle = 10
	weightTurn = 5
	weightMove = 5
)

const animPlaySec = 6 // how long a chain animation plays before the next pick

// petConfig lists a pet to spawn; each gets its own window.
type petConfig struct {
	ID string `json:"id"`
	X  int    `json:"x"` // window top-left, logical screen coords
	Y  int    `json:"y"`
}

var defaultPets = []petConfig{
	{ID: "main", X: 700, Y: 220},
}

// appConfig is the on-disk user config (~/.config/dsh-pet-mygo/config.json).
// Edit it (or drag the pet, which persists its position) and restart.
type appConfig struct {
	Size float64     `json:"size"` // overall pet scale, 1 = native canvas (640x360)
	Pets []petConfig `json:"pets"`
}

// configPath is the user config location; DSH_PET_MYGO_CONFIG overrides it.
func configPath() string {
	if p := os.Getenv("DSH_PET_MYGO_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".config", "dsh-pet-mygo", "config.json")
}

// loadConfig reads the user config, falling back to defaults. On first run it
// writes a default file so the user has something to edit.
func loadConfig() appConfig {
	cfg := appConfig{Size: 0.7, Pets: defaultPets}
	data, err := os.ReadFile(configPath())
	if err != nil {
		saveConfig(cfg)
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.Pets) == 0 {
		log.Printf("config %s invalid, using defaults", configPath())
		cfg = appConfig{Size: 0.7, Pets: defaultPets}
	}
	if cfg.Size <= 0 {
		cfg.Size = 1
	}
	return cfg
}

func saveConfig(cfg appConfig) {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("config dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("config write: %v", err)
	}
}

// field mirrors dsh-pet's state leaf: { counter, data }. The counter is a
// strictly-increasing millisecond timestamp; consumers compare it to detect
// change, exactly as they do against the real dsh-pet.
type field struct {
	Counter int64       `json:"counter"`
	Data    interface{} `json:"data"`
}

// petState is the per-pet slice of /state the bridge extension reads.
type petState struct {
	Say    field `json:"say"`
	Anim   field `json:"anim"`
	Facing field `json:"facing"` // "left" or "right"; the page mirrors the video on left
}

// newPetState returns a pet's initial state.
func newPetState() *petState {
	return &petState{
		Anim:   field{Counter: 0, Data: map[string]any{"name": idleAnim}},
		Say:    field{Counter: 0, Data: nil},
		Facing: field{Counter: 0, Data: "right"},
	}
}

type fullState struct {
	Sections map[string]field    `json:"sections"`
	Pets     map[string]petState `json:"pets"`
}

// playback is the receipt for an animation command: accepted by the host,
// then reported by the page as playing/completed/error.
type playback struct {
	ID      string `json:"id"`
	Pet     string `json:"pet"`
	Name    string `json:"name"`
	Status  string `json:"status"` // queued | accepted | playing | completed | error
	Counter int64  `json:"counter"`
}

// animSlot is the animation on screen and when its clip ends. The chain
// schedules against it, so a clip is never cut in half.
type animSlot struct {
	name  string
	pid   string
	until time.Time
}

// pendingAnim is a client-requested animation waiting for the current clip.
type pendingAnim struct {
	name       string
	playbackID string
}

type server struct {
	mu          sync.Mutex
	pets        map[string]*petState
	windows     map[string]*mygo.Window // pet id -> window
	winPet      map[*mygo.Window]string // window -> pet id
	dragGen     map[*mygo.Window]int64  // bumped on manual move, cancels an in-flight fling
	playbacks   map[string]*playback
	petHome     map[string][2]int      // pet id -> initial (x, y)
	menuOpen    map[*mygo.Window]bool  // right-click menu open per window
	cfg         appConfig              // loaded user config; positions persisted back
	hitBox      map[string][4]int      // pet id -> current animation's opaque bbox
	cur         map[string]animSlot    // pet id -> animation on screen + clip end
	pending     map[string]pendingAnim // pet id -> requested animation waiting for a free slot
	lastReq     map[string]bool        // pet id -> last clip played was requested, not random
	pageSeen    map[string]time.Time   // pet id -> last heartbeat from the rendering page
	lastReload  map[string]time.Time   // pet id -> last page reload, to debounce recovery
	lastWhisper string                 // last muttered line, to avoid immediate repeats
}

func newServer() *server {
	s := &server{
		pets:       map[string]*petState{"main": newPetState()},
		windows:    map[string]*mygo.Window{},
		winPet:     map[*mygo.Window]string{},
		dragGen:    map[*mygo.Window]int64{},
		playbacks:  map[string]*playback{},
		petHome:    map[string][2]int{},
		menuOpen:   map[*mygo.Window]bool{},
		cfg:        loadConfig(),
		hitBox:     map[string][4]int{},
		cur:        map[string]animSlot{},
		pending:    map[string]pendingAnim{},
		lastReq:    map[string]bool{},
		pageSeen:   map[string]time.Time{},
		lastReload: map[string]time.Time{},
	}
	applyScale(s.cfg.Size)
	return s
}

// addWindow registers a window for a pet, seeds its state, and remembers its
// home position.
func (s *server) addWindow(id string, w *mygo.Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows[id] = w
	s.winPet[w] = id
	s.pageSeen[id] = time.Now()
	if s.pets[id] == nil {
		s.pets[id] = newPetState()
	}
	x, y := w.Position()
	s.petHome[id] = [2]int{x, y}
}

func (s *server) windowFor(id string) *mygo.Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windows[id]
}

func (s *server) setAnim(id, name string) string {
	return s.setAnimID(id, name, "")
}

// setAnimID switches the pet's animation, reusing playbackID when the play was
// queued earlier, and records when the clip ends.
func (s *server) setAnimID(id, name, playbackID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAnimLocked(id, name, playbackID)
}

// setAnimLocked is setAnimID for callers that already hold s.mu.
func (s *server) setAnimLocked(id, name, playbackID string) string {
	pid := playbackID
	if pid == "" {
		pid = fmt.Sprintf("pb-%d", time.Now().UnixNano())
	}
	p := s.pets[id]
	if p == nil {
		p = newPetState()
		s.pets[id] = p
	}
	now := time.Now()
	p.Anim = field{Counter: now.UnixMilli(), Data: map[string]any{"name": name, "playbackId": pid}}
	s.playbacks[pid] = &playback{ID: pid, Pet: id, Name: name, Status: "accepted", Counter: now.UnixMilli()}
	s.cur[id] = animSlot{name: name, pid: pid, until: now.Add(animDuration(name))}
	log.Printf("anim %s -> %s (%s)", id, name, pid)
	return pid
}

// requestAnim plays a client-requested animation now when the pet is free and
// queues it behind the current clip otherwise, so requested animations (work
// status, whispers) don't chop a random one in half.
func (s *server) requestAnim(id, name string) string {
	s.mu.Lock()
	slot := s.cur[id]
	busy := time.Now().Before(slot.until)
	// Already on screen (a session status flapping between two tiers): keep the
	// clip running instead of restarting it, so it doesn't get cut short.
	if busy && slot.name == name {
		s.mu.Unlock()
		return slot.pid
	}
	if busy {
		if p, ok := s.pending[id]; ok && p.name == name {
			s.mu.Unlock()
			return p.playbackID
		}
	}
	s.mu.Unlock()
	if !busy {
		pid := s.setAnimID(id, name, "")
		s.markRequestPlayed(id)
		return pid
	}
	pid := fmt.Sprintf("pb-%d", time.Now().UnixNano())
	s.mu.Lock()
	s.playbacks[pid] = &playback{ID: pid, Pet: id, Name: name, Status: "queued", Counter: time.Now().UnixMilli()}
	s.pending[id] = pendingAnim{name: name, playbackID: pid}
	s.mu.Unlock()
	log.Printf("anim %s -> %s queued (%s)", id, name, pid)
	return pid
}

// markRequestPlayed records that a client-requested clip took the screen.
func (s *server) markRequestPlayed(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReq[id] = true
}

// advance moves the pet to its next animation at a clip boundary: the queued
// request when it's the request's turn, otherwise a fresh chain pick. Requests
// and random picks alternate, so a busy session can't crowd out the random pool
// and the random pool can't ignore the session state. Decision and play happen
// under one lock, so a request landing on the boundary is never cut in half.
func (s *server) advance(id string) (kind, name string, played bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, hasPending := s.pending[id]; hasPending && !s.lastReq[id] {
		delete(s.pending, id)
		s.lastReq[id] = true
		s.setAnimLocked(id, p.name, p.playbackID)
		return "", "", false
	}
	if time.Now().Before(s.cur[id].until) {
		return "", "", false
	}
	kind, name = s.pickNext()
	s.lastReq[id] = false
	if kind == "move" {
		// roam drives its own walk animations.
		return kind, name, true
	}
	s.setAnimLocked(id, name, "")
	return kind, name, true
}

// waitClip blocks until the clip on screen has played out.
func (s *server) waitClip(id string) {
	for {
		s.mu.Lock()
		slot := s.cur[id]
		s.mu.Unlock()
		left := time.Until(slot.until)
		if left <= 0 {
			return
		}
		if left > 200*time.Millisecond {
			left = 200 * time.Millisecond
		}
		time.Sleep(left)
	}
}

func (s *server) setSay(id, text string, duration int) {
	if duration <= 0 {
		duration = 8000
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pets[id]
	if p == nil {
		p = newPetState()
		s.pets[id] = p
	}
	p.Say = field{Counter: time.Now().UnixMilli(), Data: map[string]any{"text": text, "duration": duration}}
	log.Printf("say %s -> %q", id, text)
}

func (s *server) setFacing(id, facing string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pets[id]
	if p == nil {
		p = newPetState()
		s.pets[id] = p
	}
	p.Facing = field{Counter: time.Now().UnixMilli(), Data: facing}
	log.Printf("facing %s -> %s", id, facing)
}

// reportPlayback records a playback status reported by the rendering page.
func (s *server) reportPlayback(id, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pb := s.playbacks[id]
	if pb == nil {
		return false
	}
	pb.Status = status
	pb.Counter = time.Now().UnixMilli()
	return true
}

func (s *server) state() fullState {
	s.mu.Lock()
	defer s.mu.Unlock()
	pets := make(map[string]petState, len(s.pets))
	for id, p := range s.pets {
		pets[id] = petState{Say: p.Say, Anim: p.Anim, Facing: p.Facing}
	}
	return fullState{
		// Sections exist for shape-compatibility; this port has no balance or
		// work-status source, so they stay idle like dsh-pet's standalone mode.
		Sections: map[string]field{
			"balance":    {Counter: 0, Data: nil},
			"workStatus": {Counter: 0, Data: nil},
			"notify":     {Counter: 0, Data: nil},
		},
		Pets: pets,
	}
}

// config returns the animation pools under dsh-pet's config shape, so the pi
// bridge extension's loadWorkAnims() reads a real pool and the right-click menu
// can list animations by category.
func (s *server) config() map[string]any {
	return map[string]any{
		"main": map[string]any{
			"animations": map[string]any{
				"idle":    []string{idleAnim},
				"turn":    []string{"东张西望"},
				"moves":   roamAnims,
				"clicks":  clickAnims,
				"actions": s.actionPool(),
				"events":  map[string]any{"workStatus": workStatusAnims},
			},
		},
	}
}

// beginMove bumps the window's drag generation so any active fling or roam
// stops, and returns the new generation for the move that's about to run.
func (s *server) beginMove(w *mygo.Window) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dragGen[w]++
	return s.dragGen[w]
}

// move places a pet's window at normalized coordinates (0..1) over the primary
// workAreaFor returns the work area of the display the window is on, so a pet
// placed on a secondary monitor roams and bounces within that screen.
func (s *server) workAreaFor(w *mygo.Window) mygo.Rectangle {
	x, y := w.Position()
	d := mygo.Screen.DisplayMatching(mygo.Rectangle{X: x, Y: y, Width: windowW, Height: windowH})
	return d.WorkArea
}

// display's work area.
func (s *server) move(id string, x, y float64) {
	win := s.windowFor(id)
	if win == nil {
		return
	}
	s.beginMove(win)
	wa := s.workAreaFor(win)
	nx := float64(wa.X) + x*float64(wa.Width-windowW)
	ny := float64(wa.Y) + y*float64(wa.Height-windowH)
	win.SetPosition(int(nx), int(ny))
}

// fling moves a window under an initial velocity (px/s), bouncing off work-area
// edges with friction until it rests or a manual move cancels it.
func (s *server) fling(win *mygo.Window, vx, vy float64) {
	wa := s.workAreaFor(win)
	gen := s.beginMove(win)

	x, y := win.Position()
	fx, fy := float64(x), float64(y)
	minX := float64(wa.X)
	maxX := float64(wa.X + wa.Width - windowW)
	minY := float64(wa.Y)
	maxY := float64(wa.Y + wa.Height - windowH)

	const dt = 1.0 / 60.0
	step := func() bool {
		s.mu.Lock()
		cancelled := s.dragGen[win] != gen
		s.mu.Unlock()
		if cancelled {
			return true
		}
		fx += vx * dt
		fy += vy * dt
		if fx < minX {
			fx = minX
			vx = -vx * 0.82
		} else if fx > maxX {
			fx = maxX
			vx = -vx * 0.82
		}
		if fy < minY {
			fy = minY
			vy = -vy * 0.82
		} else if fy > maxY {
			fy = maxY
			vy = -vy * 0.82
		}
		win.SetPosition(int(fx), int(fy))
		vx *= 0.986
		vy *= 0.986
		return math.Abs(vx) < 6 && math.Abs(vy) < 6
	}
	for {
		if step() {
			return
		}
		time.Sleep(16 * time.Millisecond)
	}
}

// throw flings a pet by angle (degrees, 0 = right, 90 = down) and power.
func (s *server) throw(id string, power, angleDeg float64) {
	win := s.windowFor(id)
	if win == nil {
		return
	}
	rad := angleDeg * math.Pi / 180
	speed := power * 2200
	s.fling(win, speed*math.Cos(rad), speed*math.Sin(rad))
}

// roam walks a pet a short distance in a random direction, probing the work-area
// edges so it never walks off screen. The walk animation plays while the window
// slides, then the pet returns to idle.
func (s *server) roam(id, anim string) {
	win := s.windowFor(id)
	if win == nil {
		return
	}
	wa := s.workAreaFor(win)
	x, y := win.Position()

	leftSpace := x - wa.X
	rightSpace := (wa.X + wa.Width - windowW) - x
	const minSpace = 200
	var dirs []int
	if leftSpace > minSpace {
		dirs = append(dirs, -1)
	}
	if rightSpace > minSpace {
		dirs = append(dirs, 1)
	}
	if len(dirs) == 0 {
		return
	}
	dir := dirs[rand.Intn(len(dirs))]

	room := leftSpace
	if dir > 0 {
		room = rightSpace
	}
	maxDist := room - 20
	if maxDist < 60 {
		maxDist = 60
	}
	dist := 60 + rand.Intn(maxDist-60+1)

	facing := "right"
	if dir < 0 {
		facing = "left"
	}
	s.setFacing(id, facing)
	s.setAnim(id, anim)
	gen := s.beginMove(win)

	const moveDur = 2000 * time.Millisecond
	const steps = 24
	stepDur := moveDur / steps
	for i := 1; i <= steps; i++ {
		s.mu.Lock()
		cancelled := s.dragGen[win] != gen
		s.mu.Unlock()
		if cancelled {
			return
		}
		win.SetPosition(x+dir*dist*i/steps, y)
		time.Sleep(stepDur)
	}
	s.setAnim(id, idleAnim)
}

// autoRoam periodically walks a pet while it is idle, until the process ends.
func (s *server) autoRoam(id string) {
	for {
		time.Sleep(time.Duration(10+rand.Intn(10)) * time.Second)
		s.roam(id, roamAnims[rand.Intn(len(roamAnims))])
	}
}

// toggleFacing flips the pet's facing after a turn animation.
func (s *server) toggleFacing(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pets[id]
	if p == nil {
		return
	}
	cur := "right"
	if f, ok := p.Facing.Data.(string); ok {
		cur = f
	}
	next := "left"
	if cur == "left" {
		next = "right"
	}
	p.Facing = field{Counter: time.Now().UnixMilli(), Data: next}
	log.Printf("facing %s -> %s (turn)", id, next)
}

// isChainExcluded reports whether an animation is reserved for events or
// interactions and should not appear in the random action pool.
func isChainExcluded(name string) bool {
	for _, p := range []string{"点击回应-", "工作状态-", "碎碎念-", "余额-"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	switch name {
	case idleAnim, "东张西望", "被鼠标拖拽悬空反馈", "螃蟹走路", "原地漂浮踏步", "原地左转奔跑":
		return true
	}
	return false
}

var (
	actionPoolOnce  sync.Once
	actionPoolCache []string
)

// actionPool is the set of random-action animations, scanned once from the
// assets dir minus the reserved idle/turn/move/clicks/events ones.
func (s *server) actionPool() []string {
	actionPoolOnce.Do(func() {
		entries, err := os.ReadDir(animDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, animExt) {
				continue
			}
			base := strings.TrimSuffix(name, animExt)
			if isChainExcluded(base) {
				continue
			}
			actionPoolCache = append(actionPoolCache, base)
		}
	})
	return actionPoolCache
}

// pickNext chooses the next chain animation by weight: idle / turn / move /
// random action.
func (s *server) pickNext() (kind, name string) {
	r := rand.Intn(100)
	switch {
	case r < weightIdle:
		return "idle", idleAnim
	case r < weightIdle+weightTurn:
		return "turn", "东张西望"
	case r < weightIdle+weightTurn+weightMove:
		return "move", roamAnims[rand.Intn(len(roamAnims))]
	default:
		pool := s.actionPool()
		if len(pool) == 0 {
			return "idle", idleAnim
		}
		return "action", pool[rand.Intn(len(pool))]
	}
}

// findAtom returns the payload of the named atom among data's top-level atoms.
func findAtom(data []byte, name string) ([]byte, bool) {
	off := 0
	for off+8 <= len(data) {
		size := uint64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if size == 1 {
			if off+16 > len(data) {
				break
			}
			size = binary.BigEndian.Uint64(data[off+8:])
		}
		if size < 8 || off+int(size) > len(data) {
			break
		}
		if typ == name {
			// Return the payload (skip the size+type header) so nested lookup
			// walks the child atoms, not the parent itself.
			return data[off+8 : off+int(size)], true
		}
		off += int(size)
	}
	return nil, false
}

// movDuration reads a QuickTime movie's duration from its mvhd atom, so the
// animation chain waits the real length instead of a fixed guess.
func movDuration(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	moov, ok := findAtom(data, "moov")
	if !ok {
		return 0, errors.New("no moov atom")
	}
	mvhd, ok := findAtom(moov, "mvhd")
	if !ok {
		return 0, errors.New("no mvhd atom")
	}
	body := mvhd // already the payload: version + flags + fields
	if len(body) < 20 {
		return 0, errors.New("short mvhd")
	}
	if body[0] == 1 { // 64-bit version
		if len(body) < 32 {
			return 0, errors.New("short mvhd v1")
		}
		timescale := binary.BigEndian.Uint32(body[20:24])
		duration := binary.BigEndian.Uint64(body[24:32])
		if timescale == 0 {
			return 0, errors.New("zero timescale")
		}
		return float64(duration) / float64(timescale), nil
	}
	timescale := binary.BigEndian.Uint32(body[12:16])
	duration := binary.BigEndian.Uint32(body[16:20])
	if timescale == 0 {
		return 0, errors.New("zero timescale")
	}
	return float64(duration) / float64(timescale), nil
}

var animDurCache sync.Map // animation name -> time.Duration

// EBML IDs of the WebM elements the duration lives in.
const (
	ebmlSegment       = 0x18538067
	ebmlInfo          = 0x1549A966
	ebmlTimecodeScale = 0x2AD7B1
	ebmlDuration      = 0x4489
)

// ebmlID reads an element ID (1-4 bytes) including its length marker.
func ebmlID(b []byte) (id uint64, n int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	n = 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 4 || len(b) < n {
		return 0, 0
	}
	for i := range n {
		id = id<<8 | uint64(b[i])
	}
	return id, n
}

// ebmlSize reads a size VINT, returning its value, its encoded length, and
// whether the element has an unknown size.
func ebmlSize(b []byte) (size uint64, n int, unknown bool, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, false
	}
	n = 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false, false
	}
	size = uint64(b[0] & (0xFF >> n))
	for i := 1; i < n; i++ {
		size = size<<8 | uint64(b[i])
	}
	// All value bits set means the element's size is not known.
	unknown = size == (uint64(1)<<(7*n))-1
	return size, n, unknown, true
}

// ebmlElements walks the elements at one level, stopping when fn returns false.
func ebmlElements(data []byte, fn func(id uint64, body []byte) bool) {
	for len(data) > 0 {
		id, idn := ebmlID(data)
		if idn == 0 {
			return
		}
		size, szn, unknown, ok := ebmlSize(data[idn:])
		if !ok {
			return
		}
		start := idn + szn
		var body []byte
		if unknown {
			body = data[start:]
		} else {
			if size > uint64(len(data)-start) {
				return
			}
			body = data[start : start+int(size)]
			data = data[start+int(size):]
		}
		if !fn(id, body) {
			return
		}
		if unknown {
			return
		}
	}
}

// ebmlFind returns the payload of the first child element with the given ID.
func ebmlFind(data []byte, want uint64) ([]byte, bool) {
	var out []byte
	found := false
	ebmlElements(data, func(id uint64, body []byte) bool {
		if id == want {
			out, found = body, true
			return false
		}
		return true
	})
	return out, found
}

// ebmlUint reads a big-endian unsigned integer payload (up to 8 bytes).
func ebmlUint(b []byte) uint64 {
	var v uint64
	if len(b) > 8 {
		b = b[len(b)-8:]
	}
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// webmDuration reads a WebM's duration from Segment > Info > Duration, which
// is in TimecodeScale units (1ms by default), so the animation chain can wait
// for the real clip length on WebKitGTK and WebView2 too.
func webmDuration(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	segment, ok := ebmlFind(data, ebmlSegment)
	if !ok {
		return 0, errors.New("no Segment element")
	}
	info, ok := ebmlFind(segment, ebmlInfo)
	if !ok {
		return 0, errors.New("no Info element")
	}
	scale := uint64(1_000_000) // nanoseconds per timecode unit
	if raw, ok := ebmlFind(info, ebmlTimecodeScale); ok {
		if v := ebmlUint(raw); v > 0 {
			scale = v
		}
	}
	raw, ok := ebmlFind(info, ebmlDuration)
	if !ok {
		return 0, errors.New("no Duration element")
	}
	var seconds float64
	switch len(raw) {
	case 4:
		seconds = float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))
	case 8:
		seconds = math.Float64frombits(binary.BigEndian.Uint64(raw))
	default:
		return 0, errors.New("bad Duration element")
	}
	if seconds <= 0 {
		return 0, errors.New("zero duration")
	}
	return seconds * float64(scale) / 1e9, nil
}

// animPath is an animation's asset file for this platform's webview.
func animPath(name string) string {
	return filepath.Join(animDir, name+animExt)
}

// animDuration returns the real playback length of an animation, falling back
// to animPlaySec if the asset can't be read.
func animDuration(name string) time.Duration {
	if v, ok := animDurCache.Load(name); ok {
		return v.(time.Duration)
	}
	var (
		secs float64
		err  error
	)
	if animExt == ".webm" {
		secs, err = webmDuration(animPath(name))
	} else {
		secs, err = movDuration(animPath(name))
	}
	if err != nil || secs <= 0 || secs > 120 {
		secs = float64(animPlaySec)
	}
	dur := time.Duration(secs * float64(time.Second))
	animDurCache.Store(name, dur)
	return dur
}

// animChain cycles the pet through animations: each clip plays its real length
// and the next one is picked by weight (idle / turn / roam / random action).
// Requested animations (work status, whispers) wait for the current clip to end
// instead of cutting it, and are played as soon as the pet is free.
func (s *server) animChain(id string) {
	for {
		s.waitClip(id)
		kind, name, played := s.advance(id)
		if !played {
			continue
		}
		log.Printf("chain %s -> %s %s (%.1fs)", id, kind, name, animDuration(name).Seconds())
		if kind == "move" {
			s.roam(id, name)
			continue
		}
		if kind == "turn" {
			s.toggleFacing(id)
		}
	}
}

// whisper has the pet mutter a line with a whisper animation. It uses the
// pre-generated cache if present (see -generate-whispers), else the built-in
// pool — the running pet never calls a model itself.
func (s *server) whisper(id string) {
	lines := whisperLines
	if custom := loadWhisperLines(); len(custom) > 0 {
		lines = custom
	}
	var line string
	for i := 0; i < 5; i++ {
		line = lines[rand.Intn(len(lines))]
		if line != s.lastWhisper || len(lines) == 1 {
			break
		}
	}
	s.mu.Lock()
	s.lastWhisper = line
	s.mu.Unlock()
	anim := whisperAnims[rand.Intn(len(whisperAnims))]
	s.requestAnim(id, anim)
	s.setSay(id, line, 6000)
}

// markPageAlive records that a pet's page is still running. The page calls
// /state every 400ms with page=1, so silence means its WKWebView content
// process is gone or wedged.
func (s *server) markPageAlive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSeen[id] = time.Now()
}

// pageWatchdog reloads a pet's page when it stops heartbeating. A WKWebView
// whose content process is killed paints nothing and never recovers on its own,
// which looks exactly like the pet vanished; a reload respawns the process.
// reloadPage reloads a pet's page, at most once every few seconds. Recovering
// from a dead content process can report more than one termination, and a
// reload storm only keeps the window blank for longer.
func (s *server) reloadPage(id string, win *mygo.Window) {
	s.mu.Lock()
	if time.Since(s.lastReload[id]) < 3*time.Second {
		s.mu.Unlock()
		return
	}
	s.lastReload[id] = time.Now()
	s.mu.Unlock()
	// Never block the caller (the termination callback runs on the main thread):
	// let the dead process finish tearing down, then respawn it.
	go func() {
		time.Sleep(200 * time.Millisecond)
		win.Page().Reload()
	}()
}

func (s *server) pageWatchdog(id string) {
	const stale = 30 * time.Second
	for {
		time.Sleep(5 * time.Second)
		s.mu.Lock()
		seen := s.pageSeen[id]
		win := s.windows[id]
		s.mu.Unlock()
		if win == nil || time.Since(seen) < stale {
			continue
		}
		log.Printf("page %s silent for %s, reloading", id, time.Since(seen).Round(time.Second))
		// Reset the heartbeat so a page that fails to come back is retried
		// every stale interval instead of reloading in a tight loop.
		s.markPageAlive(id)
		s.reloadPage(id, win)
	}
}

// whisperLoop periodically makes the pet mutter to itself.
func (s *server) whisperLoop(id string) {
	for {
		time.Sleep(time.Duration(60+rand.Intn(60)) * time.Second)
		s.whisper(id)
	}
}

// Pet is the service bound to each page via mygo.Bind. Bridge methods take the
// calling window from the context, so each pet's page moves its own window.
type Pet struct{ server *server }

// Ping lets a page verify the bridge is wired up.
func (p *Pet) Ping() string { return "pong" }

// WhoAmI returns the pet id of the window that called it.
func (p *Pet) WhoAmI(ctx context.Context) string {
	w := mygo.CallerWindow(ctx)
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if id, ok := p.server.winPet[w]; ok {
		return id
	}
	return "main"
}

// Click plays a random click-response animation; the page calls it on a short
// press (as opposed to a drag).
func (p *Pet) Click(ctx context.Context) {
	w := mygo.CallerWindow(ctx)
	p.server.mu.Lock()
	id := p.server.winPet[w]
	p.server.mu.Unlock()
	if id == "" {
		id = "main"
	}
	p.server.setAnim(id, clickAnims[rand.Intn(len(clickAnims))])
	// A click is instant feedback: it preempts the current clip and counts as
	// the request turn, so the chain gets the next random pick.
	p.server.markRequestPlayed(id)
}

// MoveTo moves the calling window's top-left corner to a screen position.
func (p *Pet) MoveTo(ctx context.Context, x, y int) {
	w := mygo.CallerWindow(ctx)
	p.server.beginMove(w)
	w.SetPosition(x, y)
}

// Home moves the calling window back to its spawn position.
func (p *Pet) Home(ctx context.Context) {
	w := mygo.CallerWindow(ctx)
	p.server.mu.Lock()
	id := p.server.winPet[w]
	home := p.server.petHome[id]
	p.server.mu.Unlock()
	p.server.beginMove(w)
	w.SetPosition(home[0], home[1])
}

// SetMenuOpen tells the input-region loop the right-click menu is open, so
// the whole window keeps receiving the mouse while the user picks an item.
func (p *Pet) SetMenuOpen(ctx context.Context, open bool) {
	w := mygo.CallerWindow(ctx)
	p.server.mu.Lock()
	p.server.menuOpen[w] = open
	p.server.mu.Unlock()
	p.server.applyInputRegion(w, p.server.petID(w))
}

// SetHitBox tells the input-region loop the pet's opaque region for the
// current animation (window coords), so the transparent margin passes clicks
// through instead of the fixed fallback box.
func (p *Pet) SetHitBox(ctx context.Context, minX, minY, maxX, maxY int) {
	w := mygo.CallerWindow(ctx)
	s := p.server
	id := s.petID(w)
	s.mu.Lock()
	s.hitBox[id] = [4]int{minX, minY, maxX, maxY}
	s.mu.Unlock()
	s.applyInputRegion(w, id)
}

// Fling throws the calling window with an initial velocity (px/s), from a drag
// release.
func (p *Pet) Fling(ctx context.Context, vx, vy float64) {
	log.Printf("fling vx=%.0f vy=%.0f", vx, vy)
	p.server.fling(mygo.CallerWindow(ctx), vx, vy)
}

// petID returns the pet id of a window, falling back to "main".
func (s *server) petID(w *mygo.Window) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.winPet[w]; ok {
		return id
	}
	return "main"
}

// applyInputRegion limits the window's mouse-taking area to the pet's opaque
// box, or the whole window while the context menu is open. Linux applies it as
// the window's input shape (X11 SHAPE, Wayland wl_surface input region);
// macOS and Windows poll the pointer in MyGo. Everything outside passes clicks
// through to the desktop, so the transparent margin never blocks other apps.
func (s *server) applyInputRegion(w *mygo.Window, id string) {
	if w == nil {
		return
	}
	s.mu.Lock()
	menuOpen := s.menuOpen[w]
	minX, minY, maxX, maxY := hitMinX, hitMinY, hitMaxX, hitMaxY
	if hb, ok := s.hitBox[id]; ok {
		minX, minY, maxX, maxY = hb[0], hb[1], hb[2], hb[3]
	}
	s.mu.Unlock()
	if menuOpen {
		w.SetInputRegion([]mygo.Rectangle{{Width: windowW, Height: windowH}})
		return
	}
	w.SetInputRegion([]mygo.Rectangle{{
		X:      minX,
		Y:      minY,
		Width:  maxX - minX,
		Height: maxY - minY,
	}})
}

// savePositions writes current window positions back to the config if they
// changed, so a restart puts each pet where the user left it.
func (s *server) savePositions() {
	s.mu.Lock()
	changed := false
	for i := range s.cfg.Pets {
		id := s.cfg.Pets[i].ID
		if w := s.windows[id]; w != nil {
			x, y := w.Position()
			if s.cfg.Pets[i].X != x || s.cfg.Pets[i].Y != y {
				s.cfg.Pets[i].X = x
				s.cfg.Pets[i].Y = y
				changed = true
			}
		}
	}
	size := s.cfg.Size
	pets := make([]petConfig, len(s.cfg.Pets))
	copy(pets, s.cfg.Pets)
	s.mu.Unlock()
	if changed {
		// Preserve Size: writing only Pets would zero it out.
		saveConfig(appConfig{Size: size, Pets: pets})
		log.Printf("persisted pet positions")
	}
}

// persistLoop periodically saves window positions so a restart restores them.
func (s *server) persistLoop() {
	for {
		time.Sleep(2 * time.Second)
		s.savePositions()
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()

	// The page is loaded over file://, so its fetches are cross-origin.
	cors := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}
	jsonReply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	postJSON := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return false
		}
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return false
		}
		return true
	}
	petOf := func(r *http.Request) string {
		if p := r.URL.Query().Get("pet"); p != "" {
			return p
		}
		return "main"
	}

	mux.HandleFunc("/dsh-pet-7340/state", cors(func(w http.ResponseWriter, r *http.Request) {
		// Only the rendering page sets page=1; bridge clients poll without it,
		// so they can't mask a dead page.
		if r.URL.Query().Get("page") == "1" {
			s.markPageAlive(petOf(r))
		}
		jsonReply(w, s.state())
	}))

	mux.HandleFunc("/dsh-pet-7340/config", cors(func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, s.config())
	}))

	mux.HandleFunc("/dsh-pet-7340/anim", cors(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !postJSON(w, r, &body) {
			return
		}
		if body.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		id := petOf(r)
		// now=1 is a manual pick (the right-click menu): play it right away
		// instead of queueing it behind the current clip.
		if r.URL.Query().Get("now") == "1" {
			pid := s.setAnim(id, body.Name)
			s.markRequestPlayed(id)
			jsonReply(w, map[string]any{"ok": true, "playbackId": pid})
			return
		}
		pid := s.requestAnim(id, body.Name)
		jsonReply(w, map[string]any{"ok": true, "playbackId": pid})
	}))

	mux.HandleFunc("/dsh-pet-7340/playback/{id}", cors(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.mu.Lock()
		pb := s.playbacks[id]
		s.mu.Unlock()
		if pb == nil {
			http.Error(w, "playback not found", http.StatusNotFound)
			return
		}
		jsonReply(w, pb)
	}))

	mux.HandleFunc("/dsh-pet-7340/playback/{id}/report", cors(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Status string `json:"status"`
		}
		if !postJSON(w, r, &body) {
			return
		}
		if !s.reportPlayback(id, body.Status) {
			http.Error(w, "playback not found", http.StatusNotFound)
			return
		}
		jsonReply(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/dsh-pet-7340/whisper", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.whisper(petOf(r))
		jsonReply(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/dsh-pet-7340/broadcast", cors(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text     string `json:"text"`
			Duration int    `json:"duration"`
		}
		if !postJSON(w, r, &body) {
			return
		}
		if body.Text == "" {
			http.Error(w, "text required", http.StatusBadRequest)
			return
		}
		s.setSay(petOf(r), body.Text, body.Duration)
		jsonReply(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/dsh-pet-7340/position", cors(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			X    float64 `json:"x"`
			Y    float64 `json:"y"`
			Mode string  `json:"mode"`
		}
		if !postJSON(w, r, &body) {
			return
		}
		body.X = math.Max(0, math.Min(1, body.X))
		body.Y = math.Max(0, math.Min(1, body.Y))
		s.move(petOf(r), body.X, body.Y)
		jsonReply(w, map[string]any{"ok": true, "x": body.X, "y": body.Y})
	}))

	mux.HandleFunc("/dsh-pet-7340/throw", cors(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Angle float64 `json:"angle"`
			Power float64 `json:"power"`
		}
		if !postJSON(w, r, &body) {
			return
		}
		if body.Power <= 0 {
			body.Power = 1
		}
		go s.throw(petOf(r), body.Power, body.Angle)
		jsonReply(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/health", cors(func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, map[string]string{"name": "dsh-pet-mygo", "status": "ok"})
	}))

	return mux
}

// removeWMBorder asks i3/sway to drop the border they draw around floating
// windows. A tiling WM decorates every floating window, a utility panel too,
// and no EWMH hint opts a window out of it; a desktop pet must not sit in a
// focus-colored box. I3SOCK/SWAYSOCK are set by the WM for the processes it
// starts, so this does nothing under other window managers.
func removeWMBorder() {
	sock := os.Getenv("I3SOCK")
	if sock == "" {
		sock = os.Getenv("SWAYSOCK")
	}
	if sock == "" {
		return
	}
	payload := []byte(`[title="^dsh-pet$"] border none`)
	msg := make([]byte, 14, 14+len(payload))
	copy(msg, "i3-ipc")
	binary.LittleEndian.PutUint32(msg[6:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(msg[10:], 0) // RUN_COMMAND
	msg = append(msg, payload...)
	// The window is not managed yet when it is created, and i3 answers
	// success for a command that matched no window, so repeat it for a few
	// seconds after the window is up.
	for range 6 {
		conn, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if err != nil {
			return
		}
		conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = conn.Write(msg)
		buf := make([]byte, 512)
		_, _ = conn.Read(buf)
		conn.Close()
		time.Sleep(900 * time.Millisecond)
	}
}

func main() {
	genWhispers := flag.Bool("generate-whispers", false, "generate whisper lines via the configured LLM and exit")
	whisperCount := flag.Int("whisper-count", 30, "how many whisper lines to generate")
	flag.Parse()
	if *genWhispers {
		if err := generateWhispers(*whisperCount); err != nil {
			log.Fatalf("generate whispers: %v", err)
		}
		return
	}

	// A desktop pet is a floating accessory, not a normal app: hide the Dock
	// icon (and the folder placeholder a raw binary gets) with the accessory
	// activation policy. Windows still show.
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)

	// MyGo turns WebKitGTK's DMA-BUF renderer off by default: it draws blank
	// windows on NVIDIA, in VMs and in containers. On a GPU host it can put
	// page compositing back on the GPU, at the cost of that compatibility.
	if runtime.GOOS == "linux" {
		switch strings.ToLower(os.Getenv("DSH_PET_DMABUF")) {
		case "on":
			mygo.App.SetDMABUFRenderer(mygo.DMABUFRendererOn)
		case "auto":
			mygo.App.SetDMABUFRenderer(mygo.DMABUFRendererAuto)
		}
	}

	s := newServer()
	mygo.Bind(&Pet{server: s})
	go s.persistLoop()
	go func() {
		log.Printf("serving /dsh-pet-7340 on http://%s", listenAddr)
		if err := http.ListenAndServe(listenAddr, s.handler()); err != nil {
			log.Fatalf("http server: %v", err)
		}
	}()

	mygo.App.WhenReady(func() {
		for _, pc := range s.cfg.Pets {
			pc := pc
			// Clamp a restored position to the display it lands on, even if the
			// config was edited to an off-screen point.
			d := mygo.Screen.DisplayMatching(mygo.Rectangle{X: pc.X, Y: pc.Y, Width: windowW, Height: windowH})
			wa := d.WorkArea
			pc.X = min(max(pc.X, wa.X), wa.X+wa.Width-windowW)
			pc.Y = min(max(pc.Y, wa.Y), wa.Y+wa.Height-windowH)
			opts := mygo.WindowOptions{
				Title:         "dsh-pet",
				Width:         windowW,
				Height:        windowH,
				X:             pc.X,
				Y:             pc.Y,
				Frameless:     true,
				Transparent:   true,
				AlwaysOnTop:   true,
				SkipTaskbar:   true,
				DisableShadow: true,
			}
			if runtime.GOOS == "linux" {
				// A GTK utility window: tiling WMs (i3, sway) keep it floating
				// instead of stretching it across a tile, and it stays on every
				// workspace as a desktop pet should.
				opts.Panel = true
			}
			win := mygo.NewWindow(opts)
			// MyGo's create-time X is clamped by AppKit for small values, so
			// place the window explicitly after creation.
			win.SetPosition(pc.X, pc.Y)
			s.addWindow(pc.ID, win)
			// Take the mouse on the pet's body only; the page narrows this to
			// the live animation's opaque pixels as it plays.
			s.applyInputRegion(win, pc.ID)
			if err := win.Page().LoadFile("resources/index.html"); err != nil {
				log.Fatalf("load pet page: %v", err)
			}
			// MyGo reports a dead web content process but leaves the window blank,
			// so reload it: that respawns the process and restores rendering.
			win.Page().OnRenderProcessGone(func(reason string) {
				log.Printf("page %s web content gone (%s), reloading", pc.ID, reason)
				s.markPageAlive(pc.ID)
				s.reloadPage(pc.ID, win)
			})
			go s.animChain(pc.ID)
			go s.whisperLoop(pc.ID)
			go s.pageWatchdog(pc.ID)
		}
		if runtime.GOOS == "linux" {
			go removeWMBorder()
		}
	})
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		s.savePositions()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
