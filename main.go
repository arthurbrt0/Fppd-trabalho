// Jogo do ladrão no terminal (Go + tcell).
package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
)

const (
	innerW       = 56
	innerH       = 16
	maxLives     = 3
	startSeconds = 90
	invulnTicks  = 3
	ticksPerSec  = 8 // 8 ticks ≈ 1 segundo
)

type gamePhase int

const (
	phasePlaying gamePhase = iota
	phaseWon
	phaseLost
)

type pos struct {
	x, y int
}

type direction int

const (
	dirNone direction = iota
	dirUp
	dirDown
	dirLeft
	dirRight
)

// syncState: cópia do estado para os inimigos decidirem movimento.
type syncState struct {
	self      pos
	player    pos
	w, h      int
	frame     int64
	validDirs []direction
}

type world struct {
	w, h         int
	player       pos
	spawn        pos
	probeA       pos
	probeB       pos
	samples      map[pos]struct{}
	barriers     map[pos]struct{}
	score        int
	totalSamples int
	lives        int
	timeLeft     int
	phase        gamePhase
	loseReason   string
	playerVel    direction
	frame        int64
	invuln       int
}

func (w *world) inBounds(p pos) bool {
	return p.x >= 0 && p.x < w.w && p.y >= 0 && p.y < w.h
}

func (w *world) wall(p pos) bool {
	if p.x == 0 || p.x == w.w-1 || p.y == 0 || p.y == w.h-1 {
		return true
	}
	_, ok := w.barriers[p]
	return ok
}

func step(p pos, d direction) pos {
	switch d {
	case dirUp:
		return pos{p.x, p.y - 1}
	case dirDown:
		return pos{p.x, p.y + 1}
	case dirLeft:
		return pos{p.x - 1, p.y}
	case dirRight:
		return pos{p.x + 1, p.y}
	default:
		return p
	}
}

func manhattan(a, b pos) int {
	x := a.x - b.x
	if x < 0 {
		x = -x
	}
	y := a.y - b.y
	if y < 0 {
		y = -y
	}
	return x + y
}

func (w *world) validDirsFrom(p pos) []direction {
	all := []direction{dirUp, dirDown, dirLeft, dirRight}
	out := make([]direction, 0, 4)
	for _, d := range all {
		n := step(p, d)
		if w.inBounds(n) && !w.wall(n) {
			out = append(out, d)
		}
	}
	return out
}

func decideAlfa(s syncState) direction {
	return chaseStep(s.self, s.player, s.frame)
}

func chaseStep(from, to pos, frame int64) direction {
	dx, dy := to.x-from.x, to.y-from.y
	preferX := frame%2 == 0
	if dx != 0 && dy != 0 {
		if preferX {
			if dx > 0 {
				return dirRight
			}
			if dx < 0 {
				return dirLeft
			}
		} else {
			if dy > 0 {
				return dirDown
			}
			if dy < 0 {
				return dirUp
			}
		}
	}
	if dx > 0 {
		return dirRight
	}
	if dx < 0 {
		return dirLeft
	}
	if dy > 0 {
		return dirDown
	}
	if dy < 0 {
		return dirUp
	}
	return dirNone
}

func decideBeta(s syncState) direction {
	if len(s.validDirs) == 0 {
		return dirNone
	}
	return s.validDirs[rand.Intn(len(s.validDirs))]
}

func probeRoutine(
	ctx context.Context,
	syncCh <-chan syncState,
	intentCh chan<- direction,
	decide func(syncState) direction,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-syncCh:
			if !ok {
				return
			}
			d := decide(s)
			select {
			case intentCh <- d:
			case <-ctx.Done():
				return
			}
		}
	}
}

func clockRoutine(ctx context.Context, tickCh chan<- struct{}, interval time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case tickCh <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func pollRoutine(ctx context.Context, s tcell.Screen, evCh chan<- tcell.Event, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		ev := s.PollEvent()
		if ev == nil {
			return
		}
		switch ev.(type) {
		case *tcell.EventInterrupt:
			return
		default:
			select {
			case <-ctx.Done():
				return
			case evCh <- ev:
			}
		}
	}
}

func applyDir(w *world, p pos, d direction) pos {
	if d == dirNone {
		return p
	}
	n := step(p, d)
	if !w.inBounds(n) || w.wall(n) {
		return p
	}
	return n
}

func buildSync(w *world, id int) syncState {
	self := w.probeA
	if id == 2 {
		self = w.probeB
	}
	return syncState{
		self:      self,
		player:    w.player,
		w:         w.w,
		h:         w.h,
		frame:     w.frame,
		validDirs: w.validDirsFrom(self),
	}
}

func (w *world) snapshot() world {
	cp := *w
	cp.samples = make(map[pos]struct{}, len(w.samples))
	for p := range w.samples {
		cp.samples[p] = struct{}{}
	}
	cp.barriers = make(map[pos]struct{}, len(w.barriers))
	for p := range w.barriers {
		cp.barriers[p] = struct{}{}
	}
	return cp
}

func keyQuit(e *tcell.EventKey) bool {
	if e.Key() == tcell.KeyEscape || e.Key() == tcell.KeyCtrlC {
		return true
	}
	r := e.Rune()
	return r == 'q' || r == 'Q'
}

func applyPlayerKey(w *world, e *tcell.EventKey) {
	if w.phase != phasePlaying {
		return
	}
	switch e.Rune() {
	case 'w', 'W':
		w.playerVel = dirUp
	case 's', 'S':
		w.playerVel = dirDown
	case 'a', 'A':
		w.playerVel = dirLeft
	case 'd', 'D':
		w.playerVel = dirRight
	case ' ':
		w.playerVel = dirNone
	}
}

func (w *world) clearArea(center pos, radius int) {
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			p := pos{center.x + dx, center.y + dy}
			delete(w.barriers, p)
		}
	}
}

func buildMazeBarriers(w, h int) map[pos]struct{} {
	b := make(map[pos]struct{})
	// paredes horizontais com buracos
	for y := 3; y < h-3; y += 4 {
		for x := 2; x < w-2; x++ {
			if x%5 == 3 || x%5 == 4 {
				continue
			}
			b[pos{x, y}] = struct{}{}
		}
	}
	// pilares verticais
	for x := 6; x < w-6; x += 6 {
		for y := 2; y < h-2; y++ {
			if y%4 == 2 {
				continue
			}
			b[pos{x, y}] = struct{}{}
		}
	}
	return b
}

func (w *world) isFree(p pos) bool {
	return w.inBounds(p) && !w.wall(p) && p != w.player && p != w.probeA && p != w.probeB
}

func newWorld() *world {
	ww := innerW + 2
	hh := innerH + 2
	spawn := pos{ww / 2, hh / 2}

	barriers := buildMazeBarriers(ww, hh)
	w := &world{
		w:         ww,
		h:         hh,
		spawn:     spawn,
		player:    spawn,
		probeA:    pos{2, 2},
		probeB:    pos{ww - 3, hh - 3},
		barriers:  barriers,
		samples:   make(map[pos]struct{}),
		lives:     maxLives,
		timeLeft:  startSeconds,
		phase:     phasePlaying,
		playerVel: dirNone,
	}

	w.clearArea(spawn, 2)
	w.clearArea(w.probeA, 1)
	w.clearArea(w.probeB, 1)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	placed := 0
	for placed < 14 {
		p := pos{1 + rng.Intn(innerW), 1 + rng.Intn(innerH)}
		if !w.isFree(p) {
			continue
		}
		if _, ok := w.samples[p]; ok {
			continue
		}
		w.samples[p] = struct{}{}
		placed++
	}
	w.totalSamples = len(w.samples)
	w.score = 0
	return w
}

func (w *world) tickPlaying() {
	if w.invuln > 0 {
		w.invuln--
	}
	if w.frame%ticksPerSec == 0 && w.timeLeft > 0 {
		w.timeLeft--
	}

	w.player = applyDir(w, w.player, w.playerVel)

	if _, ok := w.samples[w.player]; ok {
		delete(w.samples, w.player)
		w.score++
	}
	if w.score >= w.totalSamples {
		w.phase = phaseWon
		return
	}
	if w.timeLeft <= 0 {
		w.phase = phaseLost
		w.loseReason = "tempo esgotado"
		return
	}
}

func (w *world) applyEnemyHits() {
	if w.invuln > 0 || w.phase != phasePlaying {
		return
	}
	hit := w.probeA == w.player || w.probeB == w.player
	if !hit {
		return
	}
	w.lives--
	w.player = w.spawn
	w.playerVel = dirNone
	w.invuln = invulnTicks
	if w.lives <= 0 {
		w.phase = phaseLost
		w.loseReason = "sem vidas"
	}
}

// coordenador: atualiza o jogo a cada tick
func coordinatorRoutine(
	ctx context.Context,
	s tcell.Screen,
	tickCh <-chan struct{},
	evCh <-chan tcell.Event,
	intentA <-chan direction,
	intentB <-chan direction,
	syncA chan<- syncState,
	syncB chan<- syncState,
	renderCh chan<- world,
) {
	defer close(syncA)
	defer close(syncB)
	defer close(renderCh)

	w := newWorld()

	select {
	case renderCh <- w.snapshot():
	case <-ctx.Done():
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tickCh:
			w.frame++

			if w.phase == phasePlaying {
				w.tickPlaying()
			}

			if w.phase != phasePlaying {
				select {
				case renderCh <- w.snapshot():
				case <-ctx.Done():
					return
				}
				continue
			}

			select {
			case syncA <- buildSync(w, 1):
			case <-ctx.Done():
				return
			}
			select {
			case syncB <- buildSync(w, 2):
			case <-ctx.Done():
				return
			}

			var da, db direction
			gotA, gotB := false, false
			for !gotA || !gotB {
				select {
				case <-ctx.Done():
					return
				case d := <-intentA:
					da, gotA = d, true
				case d := <-intentB:
					db, gotB = d, true
				case ev := <-evCh:
					switch e := ev.(type) {
					case *tcell.EventKey:
						if keyQuit(e) {
							return
						}
						applyPlayerKey(w, e)
					case *tcell.EventResize:
						s.Sync()
					}
				}
			}

			// A mais lento, B todo tick
			if w.frame%2 == 0 {
				w.probeA = applyDir(w, w.probeA, da)
			}
			w.probeB = applyDir(w, w.probeB, db)
			if w.probeA == w.probeB {
				w.probeB = applyDir(w, w.probeB, db)
			}

			w.applyEnemyHits()

			select {
			case renderCh <- w.snapshot():
			case <-ctx.Done():
				return
			}

		case ev := <-evCh:
			switch e := ev.(type) {
			case *tcell.EventKey:
				if keyQuit(e) {
					return
				}
				applyPlayerKey(w, e)
			case *tcell.EventResize:
				s.Sync()
			}
		}
	}
}

func renderRoutine(ctx context.Context, s tcell.Screen, renderCh <-chan world, wg *sync.WaitGroup, mu *sync.Mutex) {
	defer wg.Done()
	styleDef := tcell.StyleDefault
	for {
		select {
		case <-ctx.Done():
			return
		case w, ok := <-renderCh:
			if !ok {
				return
			}
			mu.Lock()
			s.Clear()
			for y := 0; y < w.h; y++ {
				for x := 0; x < w.w; x++ {
					p := pos{x, y}
					st := styleDef
					ch := ' '
					switch {
					case w.wall(p):
						ch = '#'
						st = st.Foreground(tcell.ColorGray)
					case p == w.player:
						ch = '$'
						st = st.Foreground(tcell.ColorYellow).Bold(true)
					case p == w.probeA:
						ch = 'A'
						st = st.Foreground(tcell.ColorRed)
					case p == w.probeB:
						ch = 'B'
						st = st.Foreground(tcell.ColorFuchsia)
					default:
						if _, ok := w.samples[p]; ok {
							ch = '.'
							st = st.Foreground(tcell.ColorGold)
						} else {
							ch = ' '
						}
					}
					s.SetContent(x, y, ch, nil, st)
				}
			}
			_, sh := s.Size()
			statusY := sh - 2
			if statusY < 0 {
				statusY = 0
			}
			statusY2 := sh - 1
			if statusY2 < 0 {
				statusY2 = 0
			}

			line1 := fmt.Sprintf(" LADRÃO | pontos %d/%d | vidas %d | tempo %ds | WASD mover | Q sair ",
				w.score, w.totalSamples, w.lives, w.timeLeft)
			line2 := " Capture todos os pontos e sobreviva! "
			switch w.phase {
			case phaseWon:
				line2 = " *** VITÓRIA! Todos os pontos capturados! *** "
			case phaseLost:
				line2 = fmt.Sprintf(" *** DERROTA (%s)! Pressione Q para sair *** ", w.loseReason)
			}

			drawLine(s, 0, statusY, line1, styleDef.Foreground(tcell.ColorWhite))
			drawLine(s, 0, statusY2, line2, styleDef.Foreground(tcell.ColorAqua))
			s.Show()
			mu.Unlock()
		}
	}
}

func drawLine(s tcell.Screen, x, y int, text string, st tcell.Style) {
	for i, r := range text {
		s.SetContent(x+i, y, r, nil, st)
	}
}

func main() {
	s, err := tcell.NewScreen()
	if err != nil {
		panic(err)
	}
	if err := s.Init(); err != nil {
		panic(err)
	}
	defer s.Fini()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var renderMu sync.Mutex

	tickCh := make(chan struct{}, 1)
	evCh := make(chan tcell.Event, 32)
	intentA := make(chan direction, 1)
	intentB := make(chan direction, 1)
	syncA := make(chan syncState, 1)
	syncB := make(chan syncState, 1)
	renderCh := make(chan world, 1)

	shutdown := func() {
		cancel()
		// libera o PollEvent ao sair
		_ = s.PostEvent(tcell.NewEventInterrupt(nil))
	}

	wg.Add(1)
	go clockRoutine(ctx, tickCh, 120*time.Millisecond, &wg)

	wg.Add(1)
	go pollRoutine(ctx, s, evCh, &wg)

	wg.Add(1)
	go probeRoutine(ctx, syncA, intentA, decideAlfa, &wg)

	wg.Add(1)
	go probeRoutine(ctx, syncB, intentB, decideBeta, &wg)

	wg.Add(1)
	go renderRoutine(ctx, s, renderCh, &wg, &renderMu)

	wg.Add(1)
	go func() {
		defer wg.Done()
		coordinatorRoutine(ctx, s, tickCh, evCh, intentA, intentB, syncA, syncB, renderCh)
		shutdown()
	}()

	wg.Wait()
}
