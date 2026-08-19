// Package survey is the Survey leaf type (KD-K26 phase three (b), PR-KA-D14),
// sunk from the root messageloop package so Runtime.Survey can name the
// result type without an import cycle. Node-side survey orchestration
// (register/wait/fan-out) lives on Node in internal/runtime (D15).
package survey
