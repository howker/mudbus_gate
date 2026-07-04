package modbus

import (
	"errors"
	"fmt"
)

const (
	ExceptionIllegalFunction     byte = 0x01
	ExceptionIllegalDataAddress  byte = 0x02
	ExceptionIllegalDataValue    byte = 0x03
	ExceptionSlaveDeviceFailure  byte = 0x04
	ExceptionAcknowledge         byte = 0x05
	ExceptionSlaveDeviceBusy     byte = 0x06
	ExceptionNegativeAcknowledge byte = 0x07
	ExceptionMemoryParityError   byte = 0x08
)

type ExceptionError struct {
	Code byte
}

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("modbus exception: code 0x%02X", e.Code)
}

func (e *ExceptionError) Is(target error) bool {
	t, ok := target.(*ExceptionError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

func IsBusyError(err error) bool {
	var exc *ExceptionError
	if !errors.As(err, &exc) {
		return false
	}
	return exc.Code == ExceptionSlaveDeviceBusy
}