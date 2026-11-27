package killswitch

import (
	"bytes"
	"io"
)

func bytesContains(b []byte, s string) bool { return bytes.Contains(b, []byte(s)) }
func bytesReader(b []byte) io.Reader        { return bytes.NewReader(b) }
