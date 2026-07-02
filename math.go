package main

import (
	"regexp"
	"strings"
)

// A terminal cannot typeset math, but it can approximate it with Unicode. This
// converts a LaTeX-ish fragment (the inside of $...$) into readable Unicode:
// Greek letters, common operators, super/subscripts, roots and simple fractions.
// It is intentionally lossy — the goal is a legible one-line rendering, not
// faithful typesetting (use the HTML report for that).

var (
	reFrac      = regexp.MustCompile(`\\frac\{([^{}]*)\}\{([^{}]*)\}`)
	reSqrt      = regexp.MustCompile(`\\sqrt\{([^{}]*)\}`)
	reSupBrace  = regexp.MustCompile(`\^\{([^{}]*)\}`)
	reSupSingle = regexp.MustCompile(`\^(\\?[A-Za-z0-9+\-=()]|\\[A-Za-z]+)`)
	reSubBrace  = regexp.MustCompile(`_\{([^{}]*)\}`)
	reSubSingle = regexp.MustCompile(`_(\\?[A-Za-z0-9+\-=()]|\\[A-Za-z]+)`)
	reCmd       = regexp.MustCompile(`\\([A-Za-z]+)`)
	reSpaces    = regexp.MustCompile(`[ \t]{2,}`)
)

var mathSymbols = map[string]string{
	// greek (lower)
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ε",
	"zeta": "ζ", "eta": "η", "theta": "θ", "iota": "ι", "kappa": "κ",
	"lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ", "pi": "π", "rho": "ρ",
	"sigma": "σ", "tau": "τ", "phi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω",
	// greek (upper)
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ",
	"Pi": "Π", "Sigma": "Σ", "Phi": "Φ", "Psi": "Ψ", "Omega": "Ω",
	// operators & relations
	"times": "×", "cdot": "⋅", "div": "÷", "pm": "±", "mp": "∓",
	"leq": "≤", "le": "≤", "geq": "≥", "ge": "≥", "neq": "≠", "ne": "≠",
	"approx": "≈", "equiv": "≡", "sim": "∼", "propto": "∝", "ll": "≪", "gg": "≫",
	"sum": "∑", "prod": "∏", "int": "∫", "oint": "∮", "partial": "∂", "nabla": "∇",
	"infty": "∞", "sqrt": "√", "forall": "∀", "exists": "∃", "nexists": "∄",
	"in": "∈", "notin": "∉", "ni": "∋", "subset": "⊂", "subseteq": "⊆",
	"supset": "⊃", "supseteq": "⊇", "cup": "∪", "cap": "∩", "emptyset": "∅",
	"land": "∧", "lor": "∨", "neg": "¬", "oplus": "⊕", "otimes": "⊗",
	"rightarrow": "→", "to": "→", "leftarrow": "←", "leftrightarrow": "↔",
	"Rightarrow": "⇒", "Leftarrow": "⇐", "Leftrightarrow": "⇔", "mapsto": "↦",
	"ldots": "…", "dots": "…", "cdots": "⋯", "angle": "∠", "perp": "⊥",
	"prime": "′", "circ": "∘", "bullet": "•", "star": "⋆", "aleph": "ℵ",
	"Re": "ℜ", "Im": "ℑ", "hbar": "ℏ", "ell": "ℓ", "deg": "°",
	"langle": "⟨", "rangle": "⟩", "lVert": "‖", "rVert": "‖", "vert": "|",
}

var superscript = map[rune]rune{
	'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶',
	'7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽',
	')': '⁾', 'n': 'ⁿ', 'i': 'ⁱ', 'a': 'ᵃ', 'b': 'ᵇ', 'c': 'ᶜ', 'd': 'ᵈ',
	'e': 'ᵉ', 'f': 'ᶠ', 'g': 'ᵍ', 'h': 'ʰ', 'j': 'ʲ', 'k': 'ᵏ', 'l': 'ˡ',
	'm': 'ᵐ', 'o': 'ᵒ', 'p': 'ᵖ', 'r': 'ʳ', 's': 'ˢ', 't': 'ᵗ', 'u': 'ᵘ',
	'v': 'ᵛ', 'w': 'ʷ', 'x': 'ˣ', 'y': 'ʸ', 'z': 'ᶻ',
}

var subscript = map[rune]rune{
	'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆',
	'7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍',
	')': '₎', 'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'i': 'ᵢ', 'j': 'ⱼ', 'k': 'ₖ',
	'l': 'ₗ', 'm': 'ₘ', 'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 'r': 'ᵣ', 's': 'ₛ',
	't': 'ₜ', 'u': 'ᵤ', 'v': 'ᵥ', 'x': 'ₓ',
}

// looksMath is a heuristic to avoid mangling ordinary "$5 and $10" text: only
// treat $...$ as math if it carries a TeX marker.
func looksMath(s string) bool {
	return strings.ContainsAny(s, `\^_{}`)
}

func latexToUnicode(s string) string {
	s = reFrac.ReplaceAllString(s, "$1⁄$2")
	s = reSqrt.ReplaceAllString(s, "√($1)")

	// spacing escapes
	for _, sp := range []string{`\,`, `\;`, `\:`, `\ `} {
		s = strings.ReplaceAll(s, sp, " ")
	}
	s = strings.ReplaceAll(s, `\!`, "")

	// non-alphabetic escapes (the generic \command pass only catches \letters)
	s = strings.NewReplacer(
		`\|`, "‖", `\%`, "%", `\&`, "&", `\#`, "#",
	).Replace(s)

	// super/subscripts (braced first, then single token)
	s = reSupBrace.ReplaceAllStringFunc(s, func(m string) string {
		return toScript(reSupBrace.FindStringSubmatch(m)[1], superscript, "^")
	})
	s = reSupSingle.ReplaceAllStringFunc(s, func(m string) string {
		return toScript(reSupSingle.FindStringSubmatch(m)[1], superscript, "^")
	})
	s = reSubBrace.ReplaceAllStringFunc(s, func(m string) string {
		return toScript(reSubBrace.FindStringSubmatch(m)[1], subscript, "_")
	})
	s = reSubSingle.ReplaceAllStringFunc(s, func(m string) string {
		return toScript(reSubSingle.FindStringSubmatch(m)[1], subscript, "_")
	})

	// remaining \commands → symbol, else drop the backslash
	s = reCmd.ReplaceAllStringFunc(s, func(m string) string {
		name := reCmd.FindStringSubmatch(m)[1]
		if v, ok := mathSymbols[name]; ok {
			return v
		}
		return name
	})

	s = strings.NewReplacer("{", "", "}", "").Replace(s)
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// toScript maps every rune of a token into the super/subscript map, falling back
// to the plain marker (^ or _) plus the token when a glyph is unavailable.
func toScript(token string, table map[rune]rune, marker string) string {
	// a leftover \command inside a script — resolve it first
	if strings.HasPrefix(token, `\`) {
		if v, ok := mathSymbols[token[1:]]; ok {
			token = v
		} else {
			token = token[1:]
		}
	}
	var b strings.Builder
	for _, r := range token {
		if sr, ok := table[r]; ok {
			b.WriteRune(sr)
		} else {
			return marker + token // unavailable glyph → keep readable fallback
		}
	}
	return b.String()
}
