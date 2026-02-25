package v2

import "github.com/google/uuid"

// FrameIDGenerator generates unique IDs for Frames.
type FrameIDGenerator interface {
	Generate() string
}

// UUIDGenerator generates UUID v4 frame IDs.
type UUIDGenerator struct{}

var _ FrameIDGenerator = (*UUIDGenerator)(nil)

func (g *UUIDGenerator) Generate() string {
	return uuid.New().String()
}
