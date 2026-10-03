//go:build windows

package main

import (
	"strings"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"sftp-syncer/internal/syncer"
)

// maxLogLines caps the in-memory log history; older lines are dropped.
const maxLogLines = 500

// logLevel classifies a log line for colourisation.
type logLevel int

const (
	logLevelInfo logLevel = iota
	logLevelSuccess
	logLevelWarn
	logLevelError
)

// logLine is one timestamped entry plus its severity.
type logLine struct {
	level logLevel
	text  string
}

// logLevelFromSyncer maps a syncer.Level to the widget's logLevel.
func logLevelFromSyncer(l syncer.Level) logLevel {
	switch l {
	case syncer.LevelSuccess:
		return logLevelSuccess
	case syncer.LevelWarn:
		return logLevelWarn
	case syncer.LevelError:
		return logLevelError
	default:
		return logLevelInfo
	}
}

// logLevelColors maps severity to its RichEdit text colour.
var logLevelColors = map[logLevel]win.COLORREF{
	logLevelSuccess: win.RGB(0x1E, 0x7D, 0x32), // green
	logLevelWarn:    win.RGB(0xB8, 0x6A, 0x00), // orange/amber
	logLevelError:   win.RGB(0xC8, 0x28, 0x28), // red
}

var msfteditDLL = windows.NewLazySystemDLL("msftedit.dll")

// logView is a read-only, colourised log widget backed by RichEdit
// (MSFTEDIT_CLASS, "RICHEDIT50W", present since Windows XP SP1). The control
// HWND is created directly; walk.InitWidget then adopts it into the widget
// tree by subclassing its WndProc. If the RichEdit class is unavailable the
// widget degrades to a plain walk.TextEdit so the app stays usable.
type logView struct {
	walk.WidgetBase
	hwnd      win.HWND
	fallback  *walk.TextEdit
	textColor win.COLORREF
}

// newLogView creates the widget under parent. It returns an error only when
// even the plain-TextEdit fallback cannot be created.
func newLogView(parent walk.Container) (*logView, error) {
	lv := &logView{}
	lv.textColor = win.COLORREF(win.GetSysColor(win.COLOR_WINDOWTEXT))

	if err := msfteditDLL.Load(); err == nil {
		hwnd := win.CreateWindowEx(
			win.WS_EX_CLIENTEDGE,
			windows.StringToUTF16Ptr(win.MSFTEDIT_CLASS),
			nil,
			win.WS_CHILD|win.WS_VISIBLE|win.WS_TABSTOP|win.WS_VSCROLL|
				win.ES_MULTILINE|win.ES_READONLY|win.ES_AUTOVSCROLL|win.ES_NOHIDESEL,
			0, 0, 0, 0,
			parent.Handle(), 0, 0, nil,
		)
		if hwnd != 0 {
			lv.hwnd = hwnd
		}
	}

	if lv.hwnd != 0 {
		// Handle() reports lv.hwnd, so InitWidget adopts the existing RichEdit
		// control instead of creating a new window.
		if err := walk.InitWidget(lv, parent, "", 0, 0); err == nil {
			lv.SendMessage(win.EM_SETBKGNDCOLOR, 0,
				uintptr(win.GetSysColor(win.COLOR_WINDOW)))
			// Cap total text so the 500-line history stays cheap.
			lv.SendMessage(win.EM_EXLIMITTEXT, 0, uintptr(1<<20))
			return lv, nil
		}
		// Adoption failed: destroy the orphan control and fall through.
		win.DestroyWindow(lv.hwnd)
		lv.hwnd = 0
	}

	te, err := walk.NewTextEdit(parent)
	if err != nil {
		return nil, err
	}
	lv.fallback = te
	_ = te.SetReadOnly(true)
	return lv, nil
}

// Handle returns the underlying HWND. walk.InitWindow uses a non-zero result
// to adopt the pre-created control rather than creating a new one.
func (lv *logView) Handle() win.HWND {
	if lv.fallback != nil {
		return lv.fallback.Handle()
	}
	return lv.hwnd
}

// CreateLayoutItem implements walk.Widget; the log view greedily fills the
// space its layout cell offers.
func (lv *logView) CreateLayoutItem(ctx *walk.LayoutContext) walk.LayoutItem {
	return walk.NewGreedyLayoutItem()
}

// SetBoundsPixels keeps the adopted RichEdit control in sync with layout.
func (lv *logView) SetBoundsPixels(bounds walk.Rectangle) error {
	if lv.fallback != nil {
		return lv.fallback.SetBoundsPixels(bounds)
	}
	return lv.WidgetBase.SetBoundsPixels(bounds)
}

// appendLines appends lines with per-severity colours and scrolls to the end.
func (lv *logView) appendLines(lines []logLine) {
	if lv.fallback != nil {
		for _, l := range lines {
			lv.fallback.AppendText(l.text + "\r\n")
		}
		lv.fallback.SendMessage(win.WM_VSCROLL, win.SB_BOTTOM, 0)
		return
	}
	lv.SendMessage(win.WM_SETREDRAW, 0, 0)
	for _, l := range lines {
		lv.appendRichLine(l)
	}
	lv.scrollToEnd()
	lv.SendMessage(win.WM_SETREDRAW, 1, 0)
	win.InvalidateRect(lv.hwnd, nil, true)
}

// replaceLines re-renders the whole history (used when the cap trims lines).
func (lv *logView) replaceLines(lines []logLine) {
	if lv.fallback != nil {
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(l.text)
			b.WriteString("\r\n")
		}
		_ = lv.fallback.SetText(b.String())
		lv.fallback.SendMessage(win.WM_VSCROLL, win.SB_BOTTOM, 0)
		return
	}
	lv.SendMessage(win.WM_SETREDRAW, 0, 0)
	// Select everything and delete it.
	lv.SendMessage(win.EM_SETSEL, 0, ^uintptr(0))
	lv.SendMessage(win.EM_REPLACESEL, 0, uintptr(unsafe.Pointer(emptyStringPtr())))
	for _, l := range lines {
		lv.appendRichLine(l)
	}
	lv.scrollToEnd()
	lv.SendMessage(win.WM_SETREDRAW, 1, 0)
	win.InvalidateRect(lv.hwnd, nil, true)
}

// scrollToEnd moves the caret/selection to the end and scrolls there.
func (lv *logView) scrollToEnd() {
	end := uintptr(0x7FFFFFFF)
	lv.SendMessage(win.EM_SETSEL, end, end)
	lv.SendMessage(win.EM_SCROLLCARET, 0, 0)
}

// appendRichLine appends one line at the end with the level's colour.
func (lv *logView) appendRichLine(l logLine) {
	// Move the insertion point to the very end (INT_MAX, not -1: for RichEdit
	// EM_SETSEL, both -1 and INT_MAX mean "end", but -1 with a selection can
	// be misread; INT_MAX is unambiguous).
	end := uintptr(0x7FFFFFFF)
	lv.SendMessage(win.EM_SETSEL, end, end)

	color, ok := logLevelColors[l.level]
	if !ok {
		color = lv.textColor
	}
	cf := win.CHARFORMAT{
		CbSize:      uint32(unsafe.Sizeof(win.CHARFORMAT{})),
		DwMask:      win.CFM_COLOR,
		CrTextColor: color,
	}
	lv.SendMessage(win.EM_SETCHARFORMAT, win.SCF_SELECTION, uintptr(unsafe.Pointer(&cf)))

	if text, err := windows.UTF16FromString(l.text + "\r\n"); err == nil {
		lv.SendMessage(win.EM_REPLACESEL, 0, uintptr(unsafe.Pointer(&text[0])))
	}

	// Reset to the default colour so later uncoloured output stays neutral.
	cf.CrTextColor = lv.textColor
	lv.SendMessage(win.EM_SETCHARFORMAT, win.SCF_SELECTION, uintptr(unsafe.Pointer(&cf)))
}

var emptyUTF16 = []uint16{0}

func emptyStringPtr() *uint16 {
	return &emptyUTF16[0]
}

// LogView is the declarative wrapper used in the MainWindow definition. It
// constructs a *logView and assigns it to AssignTo.
type LogView struct {
	AssignTo **logView
}

// Create implements declarative.Widget.
func (w LogView) Create(builder *Builder) error {
	lv, err := newLogView(builder.Parent())
	if err != nil {
		return err
	}
	if w.AssignTo != nil {
		*w.AssignTo = lv
	}
	return builder.InitWidget(w, lv, func() error { return nil })
}
