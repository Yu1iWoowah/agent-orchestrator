package cli

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Login output is live only. Unknown names and unfinished values must not be
// treated as harmless merely because a character or a frame ended.
func accountCredentialKey(name string) (root, sensitive bool) {
	decoded, err := url.QueryUnescape(name)
	if err != nil || strings.ContainsRune(decoded, '%') {
		// An incomplete/ambiguous encoding is not proof of a harmless name.
		// Do not latch a partial escape, but hide its value at assignment.
		return false, true
	}
	// Normalize for matching, but never trust a non-ASCII or malformed name
	// merely because its normalized spelling looks harmless.
	root = !utf8.ValidString(decoded) || strings.IndexFunc(decoded, func(r rune) bool {
		asciiName := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("[] ./-_\"'\t\r\n", r)
		return r >= utf8.RuneSelf || !asciiName
	}) >= 0
	sensitive = root
	name = norm.NFKC.String(cases.Fold().String(norm.NFKC.String(decoded)))
	separator := func(r rune) bool { return strings.ContainsRune("[] ./-_\"'\t\r\n", r) }
	fragments := strings.FieldsFunc(name, separator)
	trailingSeparator := name != "" && separator(rune(name[len(name)-1]))
	for i, fragment := range fragments {
		for _, family := range []string{"token", "secret", "password", "passwd", "credential", "apikey", "authorization", "bearer", "cookie", "session", "auth"} {
			if strings.Contains(fragment, family) {
				root = true
			}
		}
		if strings.HasSuffix(fragment, "key") {
			sensitive = true
			// A completed *key segment stays sensitive through later path
			// segments. A prefix like keyboard is not itself a *key name.
			root = root || i < len(fragments)-1 || trailingSeparator
		}
	}
	return root, root || sensitive
}

type loginOutputMode uint8

const (
	loginVisible loginOutputMode = iota
	loginHidden
	loginHiddenJSON
)

type loginURLPart uint8

const (
	loginURLNone loginURLPart = iota
	loginURLFirstSlash
	loginURLSecondSlash
	loginURLBody
	loginURLName
	loginURLValue
)

type loginJSONKind uint8

const (
	loginJSONPending loginJSONKind = iota
	loginJSONString
	loginJSONContainer
	loginJSONScalar
	loginJSONUnknown
)

type loginJSONStep uint8

const (
	loginJSONContinue loginJSONStep = iota
	loginJSONEnded
	loginJSONReplayBoundary
	loginJSONUncertain
)

// This scanner consumes one entire JSON value, not one terminal line. Strings
// retain escapes across frames; containers retain matching structural bounds.
type loginJSONValue struct {
	kind            loginJSONKind
	stack           []rune
	quoted, escaped bool
	scalar          string
}

func (v *loginJSONValue) consume(r rune) loginJSONStep {
	switch v.kind {
	case loginJSONUnknown:
		return loginJSONContinue
	case loginJSONPending:
		if unicode.IsSpace(r) {
			return loginJSONContinue
		}
		switch {
		case r == '"':
			v.kind = loginJSONString
		case r == '{' || r == '[':
			v.kind = loginJSONContainer
			v.stack = append(v.stack, r)
		case strings.ContainsRune("-0123456789tfn", r):
			v.kind = loginJSONScalar
			v.scalar = string(r)
		default:
			return loginJSONUncertain
		}
	case loginJSONString:
		if v.escaped {
			v.escaped = false
		} else if r == '\\' {
			v.escaped = true
		} else if r == '"' {
			return loginJSONEnded
		}
	case loginJSONContainer:
		if v.quoted {
			if v.escaped {
				v.escaped = false
			} else if r == '\\' {
				v.escaped = true
			} else if r == '"' {
				v.quoted = false
			}
		} else {
			switch r {
			case '"':
				v.quoted = true
			case '{', '[':
				if len(v.stack) >= 64 {
					return loginJSONUncertain
				}
				v.stack = append(v.stack, r)
			case '}', ']':
				if len(v.stack) == 0 || (r == '}' && v.stack[len(v.stack)-1] != '{') || (r == ']' && v.stack[len(v.stack)-1] != '[') {
					return loginJSONUncertain
				}
				v.stack = v.stack[:len(v.stack)-1]
				if len(v.stack) == 0 {
					return loginJSONEnded
				}
			}
		}
	case loginJSONScalar:
		if unicode.IsSpace(r) || strings.ContainsRune(",]}", r) {
			if json.Valid([]byte(v.scalar)) {
				return loginJSONReplayBoundary
			}
			return loginJSONUncertain
		}
		if len(v.scalar) >= 128 {
			return loginJSONUncertain
		}
		v.scalar += string(r)
	}
	return loginJSONContinue
}

type accountLoginOutput struct {
	parser  *ansi.Parser
	mode    loginOutputMode
	urlPart loginURLPart

	// A name is retained until its grammar boundary, including JSON keys.
	key       string
	keyRoot   bool
	nameDepth int
	padding   int

	jsonDepth         int
	jsonQuoted        bool
	jsonEscaped       bool
	jsonStringValue   bool
	jsonValueExpected bool
	jsonKeyPending    bool
	secret            loginJSONValue

	// Only opaque blocks use line/prompt recovery; quoted JSON never does.
	lineContent  bool
	lineStart    bool
	continuation bool
	lastCR       bool
	candidate    string
	promptLine   string
}

func (w *accountLoginOutput) resetKey() {
	w.key = ""
	w.keyRoot = false
	w.nameDepth = 0
	w.padding = 0
}
func (w *accountLoginOutput) endSecret() {
	w.mode = loginVisible
	w.urlPart = loginURLNone
	w.secret = loginJSONValue{}
	w.resetKey()
	w.jsonQuoted = false
	w.jsonEscaped = false
	w.jsonStringValue = false
	w.jsonValueExpected = false
	w.jsonKeyPending = false
	w.candidate = ""
	w.promptLine = ""
	w.lineStart = false
	w.continuation = false
	w.lineContent = false
}
func (w *accountLoginOutput) hide(out *strings.Builder) {
	w.mode = loginHidden
	w.lineContent = true
	w.lineStart = false
	w.candidate = ""
	if w.jsonDepth > 0 {
		w.mode = loginHiddenJSON
		w.secret = loginJSONValue{}
		if w.jsonQuoted {
			w.secret.kind = loginJSONUnknown
			if w.jsonStringValue {
				w.secret.kind = loginJSONString
				w.secret.escaped = w.jsonEscaped
			}
		}
	}
	out.WriteString("[credential output hidden]")
}
func (w *accountLoginOutput) trackJSON(r rune) {
	if w.jsonDepth == 0 {
		if r == '{' {
			w.jsonDepth = 1
		}
		return
	}
	if w.jsonQuoted {
		if w.jsonEscaped {
			w.jsonEscaped = false
		} else if r == '\\' {
			w.jsonEscaped = true
		} else if r == '"' {
			w.jsonQuoted = false
			if w.jsonStringValue {
				w.jsonValueExpected = false
			} else {
				w.jsonKeyPending = true
			}
		}
		return
	}
	switch r {
	case '"':
		w.jsonQuoted = true
		w.jsonStringValue = w.jsonValueExpected
	case '{':
		w.jsonKeyPending = false
		w.jsonDepth++
		w.jsonValueExpected = false
	case '}':
		w.jsonKeyPending = false
		w.jsonDepth--
		w.jsonValueExpected = false
	case ',':
		w.jsonKeyPending = false
		w.jsonValueExpected = false
	case ':':
		w.jsonKeyPending = false
		w.jsonValueExpected = true
	}
}

// A recognized HTTP(S) URL has explicit component boundaries. Only query
// names enter the name classifier; host/path and previous values cannot taint
// the next name. Unknown non-URL text and JSON keep their stricter grammar.
func (w *accountLoginOutput) urlRune(out *strings.Builder, r rune) bool {
	if w.urlPart == loginURLNone {
		if r != ':' || w.jsonDepth != 0 || (!strings.EqualFold(w.key, "https") && !strings.EqualFold(w.key, "http")) {
			return false
		}
		w.resetKey()
		w.urlPart = loginURLFirstSlash
		out.WriteRune(r)
		return true
	}
	if unicode.IsSpace(r) || strings.ContainsRune("\"'<>", r) {
		if w.urlPart != loginURLName {
			w.resetKey()
		}
		w.urlPart = loginURLNone
		return false
	}
	if w.urlPart == loginURLFirstSlash || w.urlPart == loginURLSecondSlash {
		if r != '/' {
			w.urlPart = loginURLNone
			return false
		}
		w.urlPart++
		out.WriteRune(r)
		return true
	}
	if (r == '?' && w.urlPart == loginURLBody) || (r == '&' && w.urlPart != loginURLBody) || r == '#' {
		w.resetKey()
		w.urlPart = loginURLName
		out.WriteRune(r)
		return true
	}
	if w.urlPart == loginURLName {
		if r != '=' {
			return false
		}
		_, sensitive := accountCredentialKey(w.key)
		w.urlPart = loginURLValue
		if w.keyRoot || sensitive {
			w.hide(out)
		} else {
			w.resetKey()
			out.WriteRune(r)
		}
		return true
	}
	// Value-shaped credentials are still suppressed even under a public name.
	w.key += string(r)
	if len(w.key) > 3 {
		w.key = w.key[len(w.key)-3:]
	}
	if strings.EqualFold(w.key, "sk-") || w.key == "eyJ" {
		w.hide(out)
	} else {
		out.WriteRune(r)
	}
	return true
}

func (w *accountLoginOutput) visibleRune(out *strings.Builder, r rune) {
	// Public code-entry instructions may contain punctuation/parentheses.
	// Recognize the complete bounded instruction, not its last word as a key.
	if len(w.promptLine) <= 256 {
		w.promptLine += string(r)
	}
	if r == ':' && w.jsonDepth == 0 && w.urlPart == loginURLNone && len(w.promptLine) <= 256 && recognizedCodePrompt(w.promptLine) {
		w.resetKey()
		out.WriteRune(r)
		return
	}
	if w.urlRune(out, r) {
		return
	}
	if w.urlPart == loginURLNone {
		w.trackJSON(r)
	}
	switch {
	case r == '=' || (r == ':' && w.urlPart != loginURLName):
		_, sensitive := accountCredentialKey(w.key)
		if w.keyRoot || sensitive {
			w.hide(out)
			return
		}
		w.resetKey()
	case r == ' ' || r == '\t':
		if strings.EqualFold(w.key, "bearer") {
			w.hide(out)
			return
		}
		if w.nameDepth == 0 {
			w.padding++
		}
	case r == '\'' || r == '"':
		if w.nameDepth == 0 {
			w.padding++
		}
	case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r >= utf8.RuneSelf || strings.ContainsRune("_-%+.[]/", r):
		separator := strings.ContainsRune(".[]/-_+", r)
		if w.padding > 0 && !separator && !w.jsonKeyPending && (!w.jsonQuoted || w.jsonStringValue) {
			w.resetKey()
		}
		if separator {
			_, sensitive := accountCredentialKey(w.key)
			w.keyRoot = w.keyRoot || sensitive
			w.padding = 0
		}
		if r == '[' {
			w.nameDepth++
		} else if r == ']' && w.nameDepth > 0 {
			w.nameDepth--
		}
		w.key += string(r)
		if r >= utf8.RuneSelf {
			w.keyRoot = true
		}
		if strings.EqualFold(w.key, "sk-") || w.key == "eyJ" {
			w.hide(out)
			return
		}
		root, _ := accountCredentialKey(w.key)
		w.keyRoot = w.keyRoot || root
		if len(w.key) > 128 {
			w.key = w.key[len(w.key)-128:]
		}
	default:
		// Only explicit grammar boundaries start a new name. Unknown punctuation
		// cannot erase an already observed sensitive prefix.
		if strings.ContainsRune("{},?&;", r) && !w.keyRoot && w.nameDepth == 0 && (!w.jsonQuoted || w.jsonStringValue) {
			w.resetKey()
		} else {
			w.keyRoot = true
		}
	}
	out.WriteRune(r)
}

// A hidden opaque line is never guessed to be a new public field. Recovery is
// limited to an empty line or a recognizable interactive prompt. Indented
// lines remain continuations. Ordinary visible prompts are never buffered.
func recognizedCodePrompt(s string) bool {
	s = strings.TrimSpace(cases.Fold().String(norm.NFKC.String(s)))
	command := strings.HasPrefix(s, "paste ") || strings.HasPrefix(s, "enter ") || strings.HasPrefix(s, "please enter ")
	return command && strings.Contains(s, "code") && (strings.HasSuffix(s, ":") || strings.HasSuffix(s, " >"))
}

func recognizedLoginPrompt(s string) bool {
	s = cases.Fold().String(norm.NFKC.String(s))
	if recognizedCodePrompt(s) {
		return true
	}
	if strings.HasPrefix(s, "open ") || strings.HasPrefix(s, "visit ") || strings.HasPrefix(s, "go to ") {
		return strings.Contains(s, "https://") || strings.Contains(s, "device sign-in")
	}
	if strings.HasPrefix(s, "請輸入") {
		return strings.Contains(s, "授權碼:") || strings.Contains(s, "驗證碼:")
	}
	return strings.HasPrefix(s, "authentication url:") || strings.HasPrefix(s, "keyboard shortcuts:") || strings.HasPrefix(s, "session status:")
}
func (w *accountLoginOutput) hiddenRune(out *strings.Builder, r rune) {
	if !unicode.IsSpace(r) {
		w.lineContent = true
	}
	if !w.lineStart || w.continuation {
		return
	}
	if w.candidate == "" && unicode.IsSpace(r) {
		w.continuation = true
		return
	}
	if len(w.candidate)+utf8.RuneLen(r) > 256 {
		w.lineStart = false
		w.candidate = ""
		return
	}
	w.candidate += string(r)
	if recognizedLoginPrompt(w.candidate) {
		prompt := w.candidate
		w.endSecret()
		w.jsonDepth = 0
		for _, c := range prompt {
			w.visibleRune(out, c)
		}
	}
}
func (w *accountLoginOutput) consumeRune(out *strings.Builder, r rune) {
	w.lastCR = false
	switch w.mode {
	case loginHiddenJSON:
		switch w.secret.consume(r) {
		case loginJSONEnded:
			w.endSecret()
		case loginJSONReplayBoundary:
			w.endSecret()
			w.visibleRune(out, r)
		case loginJSONUncertain:
			w.mode = loginHidden
			w.lineContent = true
			w.lineStart = false
		}
	case loginHidden:
		if w.urlPart == loginURLValue && (r == '&' || r == '#') {
			w.endSecret()
			w.urlPart = loginURLName
			out.WriteRune(r)
		} else {
			w.hiddenRune(out, r)
		}
	default:
		w.visibleRune(out, r)
	}
}
func (w *accountLoginOutput) newline(out *strings.Builder, r rune) {
	w.promptLine = ""
	// A folded value is no longer a single URL token. Keep it hidden using the
	// opaque continuation rules rather than trusting a later '&' as recovery.
	w.urlPart = loginURLNone
	// A native PTY with ONLCR expands CRLF into CRCRLF. Repeated carriage
	// returns still address the same line; they are not an empty line proving
	// that a folded credential ended. A bare CR can advance once, but cannot
	// establish a blank line without another logical newline.
	if w.lastCR {
		out.WriteRune(r)
		w.lastCR = r == '\r'
		return
	}
	w.lastCR = r == '\r'
	if w.mode == loginHiddenJSON {
		switch w.secret.consume(r) {
		case loginJSONEnded, loginJSONReplayBoundary:
			w.endSecret()
		case loginJSONUncertain:
			w.mode = loginHidden
			w.lineContent = true
		}
	}
	if w.mode == loginHidden {
		if !w.lineContent {
			w.endSecret()
			w.jsonDepth = 0
		} else {
			w.lineContent = false
			w.lineStart = true
			w.continuation = false
			w.candidate = ""
		}
	} else if w.mode == loginVisible && !w.jsonKeyPending && (!w.jsonQuoted || w.jsonStringValue) {
		w.resetKey()
	}
	out.WriteRune(r)
}
func (w *accountLoginOutput) write(out io.Writer, data []byte) {
	if w.parser == nil {
		w.parser = ansi.NewParser()
		w.parser.SetDataSize(4096)
	}
	var visible strings.Builder
	w.parser.SetHandler(ansi.Handler{
		Print: func(r rune) { w.consumeRune(&visible, r) },
		Execute: func(b byte) {
			switch b {
			case '\r', '\n':
				w.newline(&visible, rune(b))
			case '\t':
				w.consumeRune(&visible, '\t')
			case '\b':
				if w.mode == loginVisible {
					w.lastCR = false
					if w.padding > 0 {
						w.padding--
					} else if w.key != "" {
						_, n := utf8.DecodeLastRuneInString(w.key)
						w.key = w.key[:len(w.key)-n]
					}
					visible.WriteByte(b)
				}
			}
		},
	})
	// ANSI/OSC payloads are never forwarded; UTF-8 and control parser state is
	// retained across frames, as are names and all unfinished sensitive values.
	w.parser.Parse(data)
	_, _ = io.WriteString(out, visible.String())
}
