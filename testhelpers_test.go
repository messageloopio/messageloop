package messageloop

// publishPub builds a Publication from the legacy (payload, isText) tuple so
// tests keep their intent after the Publication model extension (Task 12).
func publishPub(payload []byte, isText bool) *Publication {
	kind := PayloadKindBinary
	if isText {
		kind = PayloadKindText
	}
	return &Publication{Payload: payload, Kind: kind}
}
