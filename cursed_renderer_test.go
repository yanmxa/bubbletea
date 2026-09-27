package tea

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
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
	// Summed rather than matched once: the move up is emitted in two pieces,
	// the rows skipped past the rewrap and the renderer's own move to the top
	// of the frame, and only their total says where the erase begins.
	var rows int
	for _, m := range cursorUpSeq.FindAllStringSubmatch(prefix, -1) {
		if m[1] == "" {
			rows++ // a bare CUU moves one row
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unreadable cursor-up count in %q: %v", m[0], err)
		}
		rows += n
	}
	return rows
}

var cursorUpSeq = regexp.MustCompile(`\x1b\[(\d*)A`)

// A narrowed terminal rewraps the frame it is holding, so the row the renderer
// counts as one is now two and the erase has further to travel to reach the top
// of the frame.
func TestCursedRenderer_resizeSkipsReflowedRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule string
		to   int
		env  []string
		want int
	}{
		// Two frame rows sit above the cursor, and the rule among them spans 99
		// of the 100 columns, so at 60 columns it covers two terminal rows.
		{name: "rewrapping terminal", rule: strings.Repeat("-", 99), to: 60, want: 3},
		// 21 double-width cells will not divide into rows of 21 columns: each
		// row takes 10 of them and leaves its last column empty, so the row
		// covers three terminal rows where dividing its width would say two.
		{name: "double-width cells", rule: strings.Repeat("你", 21), to: 21, want: 4},
		// tmux has rewrapped pane text on resize since 1.8, so it gets the
		// same count as any other rewrapping terminal.
		{name: "tmux", rule: strings.Repeat("-", 99), to: 60, want: 3,
			env: []string{"TMUX=/tmp/tmux-501/default,1,0"}},
		// GNU screen truncates instead, and moving up the extra rows would
		// erase scrollback the renderer never drew.
		{name: "screen", rule: strings.Repeat("-", 99), to: 60, want: 2,
			env: []string{"STY=12345.pts-0.host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			env := append([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}, tc.env...)
			r := newCursedRenderer(&output, env, 100, 24)

			frame := NewView("\n" + tc.rule + "\n> \n" + tc.rule + "\nstatus")
			frame.Cursor = NewCursor(2, 2)
			r.render(frame)
			if err := r.flush(false); err != nil {
				t.Fatalf("flush initial frame: %v", err)
			}

			output.Reset()
			r.resize(tc.to, 24)
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

// Leaving the alternate screen restores the cursor to where the inline frame
// left it, and the redraw erases from that frame's top. Homing the cursor
// first — the shrinking-frame park, taken while the renderer still thinks in
// alt-screen coordinates — made that erase start at the top of the screen and
// wipe every visible row of the conversation above the frame.
func TestCursedRenderer_altScreenExitDoesNotHomeTheCursor(t *testing.T) {
	var output bytes.Buffer
	env := []string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}
	r := newCursedRenderer(&output, env, 80, 24)

	inline := NewView("chat line\n> \nstatus")
	inline.Cursor = NewCursor(2, 1)
	r.render(inline)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush inline frame: %v", err)
	}

	modal := NewView(strings.Repeat("modal row\n", 20) + "Do you want to proceed?")
	modal.AltScreen = true
	r.render(modal)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush alt-screen frame: %v", err)
	}

	output.Reset()
	r.render(inline)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush after leaving the alt screen: %v", err)
	}
	out := output.String()
	_, after, ok := strings.Cut(out, "\x1b[?1049l")
	if !ok {
		t.Fatalf("no alt-screen exit in %q", out)
	}
	if strings.Contains(after, "\x1b[H") {
		t.Errorf("cursor homed on the main screen after leaving the alt screen: %q", after)
	}
}
