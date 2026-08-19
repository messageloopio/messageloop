package survey

// MaxSurveyAnswerBytes caps a single survey answer payload. A larger
// answer is delivered as a SURVEY_ANSWER_TOO_LARGE error with an empty
// payload (PR-07).
const MaxSurveyAnswerBytes = 4096

// MaxSurveyResultBytes caps the encoded size of an outbound client
// SurveyResult; answers beyond the cap are stripped of their payload and
// turned into errors (PR-07).
const MaxSurveyResultBytes = 256 * 1024
