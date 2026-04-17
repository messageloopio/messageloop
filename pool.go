package messageloop

import "sync"

var bytesPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

func getBuffer() *[]byte  { return bytesPool.Get().(*[]byte) }
func putBuffer(b *[]byte) { *b = (*b)[:0]; bytesPool.Put(b) }
