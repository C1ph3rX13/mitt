package cli

import (
	"fmt"

	"github.com/C1ph3rX13/mitt/internal/output"
)

type formatFlag struct {
	value string
}

func newFormatFlag() *formatFlag {
	return &formatFlag{value: string(output.FormatJSON)}
}

func (f *formatFlag) String() string {
	if f == nil {
		return string(output.FormatJSON)
	}
	return f.value
}

func (f *formatFlag) Set(value string) error {
	if _, err := output.ParseFormat(value); err != nil {
		return err
	}
	f.value = value
	return nil
}

func (f *formatFlag) Type() string {
	return "string"
}

func (f *formatFlag) Format() (output.Format, error) {
	format, err := output.ParseFormat(f.value)
	if err != nil {
		return "", fmt.Errorf("invalid output format: %w", err)
	}
	return format, nil
}
