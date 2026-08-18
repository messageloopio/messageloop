package messageloop

import (
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// PublicationFromPayloadV2 converts a shared.v2 payload envelope into a
// Publication, preserving the original oneof variant (Binary/Text/JSON).
// The JSON variant is marshaled through MarshalJSONStruct; an unmarshalable
// payload (e.g. NaN numbers) surfaces as an error that callers decide how to
// handle (client publish fails the request; the admin API logs and counts a
// failed attempt). A nil payload yields an empty Publication (nil Payload,
// zero Kind) and no error. Id and Metadata are copied as given.
func PublicationFromPayloadV2(id string, md map[string]string, p *sharedv2.Payload) (*Publication, error) {
	pub := &Publication{Id: id, Metadata: md}
	if p == nil {
		return pub, nil
	}
	pub.ContentType = p.ContentType
	switch data := p.Data.(type) {
	case *sharedv2.Payload_Json:
		payload, err := MarshalJSONStruct(data.Json)
		if err != nil {
			return nil, err
		}
		pub.Payload = payload
		pub.Kind = PayloadKindJSON
	case *sharedv2.Payload_Binary:
		pub.Payload = data.Binary
		pub.Kind = PayloadKindBinary
	case *sharedv2.Payload_Text:
		pub.Payload = []byte(data.Text)
		pub.Kind = PayloadKindText
	}
	return pub, nil
}
