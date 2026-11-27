package output

import "io"

// Mode is the resolved output contract for one invocation: the render format,
// the command's declared success-shape key, and the data and diagnostic sinks.
// Commands consult this one value instead of testing output flags themselves.
// Key "" is the command's default shape.
type Mode struct {
	Format Format
	Key    string
	Data   io.Writer
	Diag   io.Writer
}

// Writer renders the mode's data sink.
func (m Mode) Writer() *Writer {
	return NewWriter(m.Data, m.Format)
}
