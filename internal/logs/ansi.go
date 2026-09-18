// SPDX-License-Identifier: Apache-2.0

package logs

import "bytes"

// Build tools colour their output whether or not a terminal is attached —
// `CI=true` does not stop them, and `NO_COLOR` is honoured by some and ignored
// by the rest. The log is then read as plain text in four places (the run page,
// the SSE stream, the Check Run tail on GitHub, and the JSON API), none of
// which interpret escape sequences, so a coloured line arrives as
// "[42m[30m generating static routes [39m[49m".
//
// Dropping the sequences once, in the Writer every step's stdout and stderr
// already funnel through, keeps all four readers honest and needs no change at
// any of them.
//
// Rendering the colours in the browser instead would mean an SGR-to-span
// translator in Go for the page, a second one in JS for the stream, and still a
// stripper for the Check Run. That is the upgrade path if colour is ever worth
// three parsers; it is not worth one today.

// escCap bounds how much of an unterminated escape sequence is held before the
// filter gives up on it. A sequence that long is malformed, not output.
//
// ponytail: dropping the held bytes on overflow can eat up to escCap bytes of
// real log after a stray ESC; emit them instead if that is ever observed.
const escCap = 128

// ansiFilter removes ANSI escape sequences from a byte stream. It is stateful
// because a sequence can straddle two writes: a step's stdout arrives in
// whatever chunks the pipe hands over, not in whole lines.
type ansiFilter struct {
	esc []byte // an escape sequence still being consumed, "" between sequences
}

// filter returns p with every complete escape sequence removed, holding back a
// trailing partial one for the next call.
func (a *ansiFilter) filter(p []byte) []byte {
	if len(a.esc) == 0 && bytes.IndexByte(p, 0x1b) < 0 {
		return p
	}
	out := make([]byte, 0, len(p))
	for _, c := range p {
		if len(a.esc) == 0 {
			if c == 0x1b {
				a.esc = append(a.esc, c)
				continue
			}
			out = append(out, c)
			continue
		}
		a.esc = append(a.esc, c)
		if a.done() || len(a.esc) >= escCap {
			a.esc = a.esc[:0]
		}
	}
	return out
}

// done reports whether the bytes held so far are a whole escape sequence.
func (a *ansiFilter) done() bool {
	if len(a.esc) < 2 {
		return false
	}
	switch a.esc[1] {
	case '[': // CSI: parameters, then one final byte in 0x40-0x7e. SGR colour is this one.
		last := a.esc[len(a.esc)-1]
		return len(a.esc) > 2 && last >= 0x40 && last <= 0x7e
	case ']': // OSC: a string terminated by BEL or by ST (ESC \).
		last := a.esc[len(a.esc)-1]
		return last == 0x07 || (len(a.esc) > 2 && last == '\\' && a.esc[len(a.esc)-2] == 0x1b)
	default: // two-character escape (ESC c, ESC 7, …).
		return true
	}
}
