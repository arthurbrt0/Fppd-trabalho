// Estação Sync — simulação de terminal com concorrência em Go.
// Arquitetura: tick dedicado, leitor de eventos tcell, duas sondas autónomas,
// coordenador com select, renderizador com mutex apenas no acesso ao ecrã.
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
	innerW = 56
	innerH = 16
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

// syncState é uma fotografia imutável enviada às sondas a cada tick.
type syncState struct {
	self    pos
	player  pos
	samples []pos
	w, h    int
	frame   int64
}

type world struct {
	w, h      int
	player    pos
	probeA    pos
	probeB    pos
	samples   map[pos]struct{}
	score     int
	playerVel direction
	frame     int64
}

func (w *world) inBounds(p pos) bool {
	return p.x >= 0 && p.x < w.w && p.y >= 0 && p.y < w.h
}

func (w *world) wall(p pos) bool {
	return p.x == 0 || p.x == w.w-1 || p.y == 0 || p.y == w.h-1
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

// decideAlfa move em direção à amostra mais próxima; se não houver, patrulha o perímetro no sentido horário.
func decideAlfa(s syncState) direction {
	if len(s.samples) == 0 {
		return patrolBorder(s.self, s.w, s.h)
	}
	best := s.samples[0]
	bd := manhattan(s.self, best)
	for _, p := range s.samples[1:] {
		if d := manhattan(s.self, p); d < bd {
			bd = d
			best = p
		}
	}
	return greedyStep(s.self, best)
}

func greedyStep(from, to pos) direction {
	dx, dy := to.x-from.x, to.y-from.y
	if dx != 0 && dy != 0 {
		if rand.Intn(2) == 0 {
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

func patrolBorder(p pos, w, h int) direction {
	// Perímetro interior: rectângulo [1,w-2] x [1,h-2]
	if p.y == 1 && p.x < w-2 {
		return dirRight
	}
	if p.x == w-2 && p.y < h-2 {
		return dirDown
	}
	if p.y == h-2 && p.x > 1 {
		return dirLeft
	}
	return dirUp
}

func decideBeta(_ syncState) direction {
	opts := []direction{dirUp, dirDown, dirLeft, dirRight}
	return opts[rand.Intn(len(opts))]
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

// clockRoutine envia ticks do simulador.
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

// pollRoutine multiplexa eventos do ecrã e reencaminha para o coordenador.
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
	samps := make([]pos, 0, len(w.samples))
	for p := range w.samples {
		samps = append(samps, p)
	}
	self := w.probeA
	if id == 2 {
		self = w.probeB
	}
	return syncState{
		self:    self,
		player:  w.player,
		samples: samps,
		w:       w.w,
		h:       w.h,
		frame:   w.frame,
	}
}

func (w *world) snapshot() world {
	cp := *w
	cp.samples = make(map[pos]struct{}, len(w.samples))
	for p := range w.samples {
		cp.samples[p] = struct{}{}
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

// coordinatorRoutine: único dono do estado do mundo; usa select para multiplexar ticks, input e shutdown.
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

	// Primeiro frame para o utilizador ver o estado inicial sem esperar pelo primeiro tick.
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
			w.player = applyDir(w, w.player, w.playerVel)

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

			nextA := applyDir(w, w.probeA, da)
			nextB := applyDir(w, w.probeB, db)
			if nextA == nextB {
				nextB = w.probeB
			}
			if nextA == w.player {
				nextA = w.probeA
			}
			if nextB == w.player {
				nextB = w.probeB
			}
			w.probeA = nextA
			w.probeB = nextB

			for _, p := range []pos{w.probeA, w.probeB} {
				if _, ok := w.samples[p]; ok {
					delete(w.samples, p)
					w.score++
				}
			}

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

func newWorld() *world {
	w := innerW + 2
	h := innerH + 2
	samples := make(map[pos]struct{})
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 12; i++ {
		for tries := 0; tries < 50; tries++ {
			p := pos{1 + rng.Intn(innerW), 1 + rng.Intn(innerH)}
			if _, ok := samples[p]; ok {
				continue
			}
			samples[p] = struct{}{}
			break
		}
	}
	return &world{
		w: w, h: h,
		player:  pos{w / 2, h / 2},
		probeA:  pos{2, 2},
		probeB:  pos{w - 3, h - 3},
		samples: samples,
	}
}

// renderRoutine desenha o mundo; mutex só protege o estado de renderização (ecrã tcell).
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
						ch = '@'
						st = st.Foreground(tcell.ColorYellow)
					case p == w.probeA:
						ch = 'A'
						st = st.Foreground(tcell.ColorGreen)
					case p == w.probeB:
						ch = 'B'
						st = st.Foreground(tcell.ColorFuchsia)
					default:
						if _, ok := w.samples[p]; ok {
							ch = '*'
							st = st.Foreground(tcell.ColorAqua)
						} else {
							ch = '·'
							st = st.Foreground(tcell.ColorDarkGray)
						}
					}
					s.SetContent(x, y, ch, nil, st)
				}
			}
			_, sh := s.Size()
			statusY := sh - 1
			if statusY < 0 {
				statusY = 0
			}
			title := fmt.Sprintf(" Estação Sync | pontos: %d | WASD | espaço parar | Q/ESC sair ", w.score)
			for i, r := range title {
				s.SetContent(i, statusY, r, nil, styleDef.Foreground(tcell.ColorWhite))
			}
			s.Show()
			mu.Unlock()
		}
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
		// Desbloqueia PollEvent para a goroutine de eventos terminar ordenadamente.
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
