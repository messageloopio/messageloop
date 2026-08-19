// Package survey is the Survey leaf type (KD-K26 phase three (b), PR-KA-D14),
// sunk unchanged from the root messageloop package so Runtime.Survey can
// name the result type without an import cycle. Node-side survey orchestration
// (register/wait/fan-out) stays in the root until D15. Root callers reach
// Survey/SurveyResult/NewSurvey through the aliases in aliases.go.
package survey
