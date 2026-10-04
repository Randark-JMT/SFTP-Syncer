//go:build windows

package main

import (
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
// (MSFTEDIT_CLASS, "RICHEDIT50W", registered by msftedit.dll; present since
// Windows XP SP1). The control is created *through* walk.InitWidget by passing
// the RichEdit class name — walk cannot adopt a pre-created HWND, its
// InitWidget always calls CreateWindowEx itself, so manual creation followed
// by "adoption" silently fails. If msftedit.dll is unavailable the widget
// degrades to a plain read-only EDIT control: colourisation is skipped, but
// the log stays visible.
type logView struct {
	walk.WidgetBase
	textColor win.COLORREF
	isRich    bool

	// selLen is the total character count in EM_SETSEL coordinates: RichEdit
	// normalises each CRLF to a single paragraph-mark character, while the
	// plain EDIT (and WM_GETTEXTLENGTH) count two. Formatting ranges must use
	// the control's own indexing, so the cursor is tracked here instead of
	// asking the control.
	selLen int
}

// newLogView creates the widget under parent. It returns an error only when
// even the plain-TextEdit fallback cannot be created.
func newLogView(parent walk.Container) (*logView, error) {
	lv := &logView{}
	lv.textColor = win.COLORREF(win.GetSysColor(win.COLOR_WINDOWTEXT))

	className := "EDIT"
	if msfteditDLL.Load() == nil {
		className = win.MSFTEDIT_CLASS
		lv.isRich = true
	}

	if err := walk.InitWidget(
		lv,
		parent,
		className,
		win.WS_VISIBLE|win.WS_TABSTOP|win.WS_VSCROLL|
			win.ES_MULTILINE|win.ES_READONLY|win.ES_AUTOVSCROLL|win.ES_NOHIDESEL,
		win.WS_EX_CLIENTEDGE); err != nil {
		return nil, err
	}

	if lv.isRich {
		lv.SendMessage(win.EM_SETBKGNDCOLOR, 0,
			uintptr(win.GetSysColor(win.COLOR_WINDOW)))
		// Cap total text so the 500-line history stays cheap.
		lv.SendMessage(win.EM_EXLIMITTEXT, 0, uintptr(1<<20))
	}
	return lv, nil
}

// WndProc repairs the fallback EDIT's formatting rectangle on resize. Plain
// EDIT controls stop tracking size changes once updates were suspended with
// WM_SETREDRAW, leaving text wrapped to a stale width.
func (lv *logView) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if msg == win.WM_SIZE && !lv.isRich {
		lv.repairFormatRect()
	}
	return lv.WidgetBase.WndProc(hwnd, msg, wParam, lParam)
}

// CreateLayoutItem implements walk.Widget; the log view greedily fills the
// space its layout cell offers.
func (lv *logView) CreateLayoutItem(ctx *walk.LayoutContext) walk.LayoutItem {
	return walk.NewGreedyLayoutItem()
}

// appendLines appends lines with per-severity colours and scrolls to the end.
func (lv *logView) appendLines(lines []logLine) {
	lv.SendMessage(win.WM_SETREDRAW, 0, 0)
	for _, l := range lines {
		lv.appendRichLine(l)
	}
	lv.scrollToEnd()
	lv.SendMessage(win.WM_SETREDRAW, 1, 0)
	if !lv.isRich {
		lv.repairFormatRect()
	}
	win.InvalidateRect(lv.Handle(), nil, true)
}

// replaceLines re-renders the whole history (used when the cap trims lines).
func (lv *logView) replaceLines(lines []logLine) {
	lv.SendMessage(win.WM_SETREDRAW, 0, 0)
	// Select everything and delete it.
	lv.SendMessage(win.EM_SETSEL, 0, ^uintptr(0))
	lv.SendMessage(win.EM_REPLACESEL, 0, uintptr(unsafe.Pointer(emptyStringPtr())))
	lv.selLen = 0
	for _, l := range lines {
		lv.appendRichLine(l)
	}
	lv.scrollToEnd()
	lv.SendMessage(win.WM_SETREDRAW, 1, 0)
	if !lv.isRich {
		lv.repairFormatRect()
	}
	win.InvalidateRect(lv.Handle(), nil, true)
}

// scrollToEnd moves the caret/selection to the end and scrolls there.
func (lv *logView) scrollToEnd() {
	end := uintptr(0x7FFFFFFF)
	lv.SendMessage(win.EM_SETSEL, end, end)
	lv.SendMessage(win.EM_SCROLLCARET, 0, 0)
}

// appendRichLine appends one line at the end with the level's colour.
// Colouring formats the freshly inserted character range: setting the
// character format at a collapsed insertion point (SCF_SELECTION on an empty
// selection) only reliably affects later typing when the control has focus,
// whereas formatting an actual selection works unconditionally. The range is
// tracked in selLen (EM_SETSEL coordinates), NOT via WM_GETTEXTLENGTH: for
// RichEdit the two disagree by one character per line (CRLF vs one paragraph
// mark), which accumulates into a whole-line colour drift as the log grows.
func (lv *logView) appendRichLine(l logLine) {
	chars := len([]rune(l.text))
	eol := 2
	if lv.isRich {
		eol = 1
	}

	start := uintptr(lv.selLen)
	end := uintptr(0x7FFFFFFF)
	lv.SendMessage(win.EM_SETSEL, end, end)
	if text, err := windows.UTF16FromString(l.text + "\r\n"); err == nil {
		lv.SendMessage(win.EM_REPLACESEL, 0, uintptr(unsafe.Pointer(&text[0])))
	}
	lv.selLen += chars + eol

	color, ok := logLevelColors[l.level]
	if !ok {
		color = lv.textColor
	}
	cf := win.CHARFORMAT{
		CbSize:      uint32(unsafe.Sizeof(win.CHARFORMAT{})),
		DwMask:      win.CFM_COLOR,
		CrTextColor: color,
	}
	lv.SendMessage(win.EM_SETSEL, start, uintptr(lv.selLen-eol))
	lv.SendMessage(win.EM_SETCHARFORMAT, win.SCF_SELECTION, uintptr(unsafe.Pointer(&cf)))
}

// repairFormatRect resets the fallback EDIT's formatting rectangle to the
// current client area (inset by the control's own margins). Needed after the
// WM_SETREDRAW suspend/resume around batch appends, which leaves the plain
// EDIT wrapping text at a stale width. Only the fallback path invokes it;
// RichEdit tracks resizes correctly on its own.
func (lv *logView) repairFormatRect() {
	var rc win.RECT
	if !win.GetClientRect(lv.Handle(), &rc) {
		return
	}
	margins := lv.SendMessage(win.EM_GETMARGINS, 0, 0)
	rc.Left = int32(margins & 0xFFFF)
	rc.Right -= int32((margins >> 16) & 0xFFFF)
	lv.SendMessage(win.EM_SETRECT, 0, uintptr(unsafe.Pointer(&rc)))
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
