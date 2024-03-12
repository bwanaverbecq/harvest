package util

type Metadata struct {
	BytesRx  uint64
	NumCalls uint64
}

func (m *Metadata) Reset() {
	m.BytesRx = 0
	m.NumCalls = 0
}
