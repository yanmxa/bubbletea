package tea

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

type mouseRaceModel struct {
	i int
}

func (m *mouseRaceModel) Init() Cmd { return nil }

func (m *mouseRaceModel) Update(msg Msg) (Model, Cmd) {
	switch msg.(type) {
	case MouseClickMsg, MouseMotionMsg, MouseWheelMsg:
		m.i++
	}
	return m, nil
}

func (m *mouseRaceModel) View() View {
	return View{
		Content:   fmt.Sprintf("tick-%d\n", m.i),
		MouseMode: MouseModeCellMotion,
	}
}

// Fixes: https://github.com/charmbracelet/bubbletea/issues/1690
func TestCursedRenderer_mouseVsFlush(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	m := &mouseRaceModel{}
	p := NewProgram(
		m,
		WithContext(t.Context()),
		WithInput(pr),
		WithOutput(io.Discard),
		WithEnvironment([]string{
			"TERM=xterm-256color",
			"TERM_PROGRAM=Apple_Terminal",
		}),
		WithoutSignals(),
		WithWindowSize(80, 24),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()

	time.Sleep(150 * time.Millisecond)

	const iterations = 100
	for i := range iterations {
		switch i % 4 {
		case 0:
			p.Send(MouseClickMsg{X: i % 80, Y: i % 24, Button: MouseLeft})
		case 1:
			p.Send(MouseMotionMsg{X: i % 80, Y: i % 24})
		case 2:
			p.Send(MouseWheelMsg{X: 0, Y: 0, Button: MouseWheelUp})
		default:
			p.Send(MouseReleaseMsg{X: i % 80, Y: i % 24, Button: MouseLeft})
		}
	}

	p.Quit()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit after Quit")
	}
}

func TestCursedRenderer_insertAboveFlushesQueuedView(t *testing.T) {
	var output bytes.Buffer
	r := newCursedRenderer(
		&output,
		[]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"},
		80,
		24,
	)

	r.render(NewView("stale frame\nstale row"))
	if err := r.flush(false); err != nil {
		t.Fatalf("flush initial view: %v", err)
	}

	output.Reset()
	r.render(NewView("current frame"))
	if err := r.insertAbove("committed block"); err != nil {
		t.Fatalf("insert above: %v", err)
	}

	if r.lastView == nil || r.lastView.Content != "current frame" {
		t.Fatalf("insertAbove did not flush the queued view: %#v", r.lastView)
	}
	written := output.String()
	currentAt := strings.Index(written, "current frame")
	committedAt := strings.Index(written, "committed block")
	if currentAt < 0 || committedAt < 0 {
		t.Fatalf("output missing markers (current=%d, committed=%d): %q", currentAt, committedAt, written)
	}
	if currentAt > committedAt {
		t.Fatalf("inserted scrollback before flushing current frame: %q", written)
	}
}

// rowsMovedUp sums the cursor-up movement the renderer emits before it starts
// erasing, which is how far it reckons the top of the previous frame is.
func rowsMovedUp(t *testing.T, output string) int {
	t.Helper()

	prefix, _, found := strings.Cut(output, ansi.EraseScreenBelow)
	if !found {
		t.Fatalf("output never erased the old frame: %q", output)
	}
	var rows int
	for rest := prefix; ; {
		i := strings.Index(rest, "\x1b[")
		if i < 0 {
			return rows
		}
		rest = rest[i+2:]
		params := 0
		for params < len(rest) && !(rest[params] >= '@' && rest[params] <= '~') {
			params++
		}
		if params >= len(rest) {
			return rows
		}
		if rest[params] == 'A' {
			if params == 0 {
				rows++ // a bare CUU moves one row
			} else if n, err := strconv.Atoi(rest[:params]); err == nil {
				rows += n
			}
		}
		rest = rest[params+1:]
	}
}

// A narrowed terminal rewraps the frame it is holding, so the row the renderer
// counts as one is now two and the erase has further to travel to reach the top
// of the frame.
func TestCursedRenderer_resizeSkipsReflowedRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want int
	}{
		// Two frame rows sit above the cursor, and the rule among them spans 99
		// of the 100 columns, so at 60 columns it covers two terminal rows.
		{name: "rewrapping terminal", want: 3},
		// tmux truncates instead, and moving up the extra row would erase
		// scrollback the renderer never drew.
		{name: "tmux", env: []string{"TMUX=/tmp/tmux-501/default,1,0"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			env := append([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, tc.env...)
			r := newCursedRenderer(&output, env, 100, 24)

			rule := strings.Repeat("-", 99)
			frame := NewView("\n" + rule + "\n> \n" + rule + "\nstatus")
			frame.Cursor = NewCursor(2, 2)
			r.render(frame)
			if err := r.flush(false); err != nil {
				t.Fatalf("flush initial frame: %v", err)
			}

			output.Reset()
			r.resize(60, 24)
			r.render(frame)
			if err := r.flush(false); err != nil {
				t.Fatalf("flush after resize: %v", err)
			}

			if got := rowsMovedUp(t, output.String()); got != tc.want {
				t.Errorf("moved up %d rows to reach the top of the frame, want %d", got, tc.want)
			}
		})
	}
}
