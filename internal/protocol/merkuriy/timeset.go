package merkuriy

import (
	"fmt"
	"time"

	"mbgw/internal/codec"
)

// This file implements the "read current time" / "set time" / "correct
// time" commands from "Описание системы команд приборов учета Меркурий"
// (version 06.2024, covering 150/203.2TD/204/208/230/231/234/236/238/350):
//   - section 4.2 (Чтение журналов событий, параметр 00h): read current time
//   - section 3.10 (Установка времени, параметр 0Ch): set full time+date
//   - section 3.11 (Коррекция времени, параметр 0Dh): correct time within
//     +/-4 minutes/day (a much lower-risk operation than a full time set,
//     per the document's own framing)
//
// All time fields are BCD-encoded on the wire (codec.DecodeBCDByte /
// codec.EncodeBCDByte), consistent with every other BCD use seen in this
// device family. Year is 2 BCD digits, interpreted as 2000+yy (matching
// the convention already used for Akron-01's clock registers).
//
// NOTE: building/parsing these frames is protocol-level plumbing only.
// Deciding *when* and *how* an operator or the gateway itself is allowed
// to invoke BuildSetTime/BuildCorrectTime against a live metering device
// (access control, confirmation, audit logging) is a separate, higher-level
// design question - deliberately not addressed here.

const (
	paramReadCurrentTime = 0x00
	paramSetTime         = 0x0C
	paramCorrectTime     = 0x0D

	codeReadParameter  = 0x04
	codeWriteParameter = 0x03
)

// BuildReadParameter builds a generic "read parameter" request (command
// code 0x04): [addr][0x04][paramNumber][CRC]. Section 4.2's Рисунок 4.1.
func BuildReadParameter(addr byte, paramNumber byte) []byte {
	return BuildFrame(addr, codeReadParameter, []byte{paramNumber})
}

// BuildWriteParameter builds a generic "write parameter" request (command
// code 0x03): [addr][0x03][paramNumber][data...][CRC]. Section 3's write
// command family (e.g. Рисунок 3.34 and the section 3.10/3.11 examples).
func BuildWriteParameter(addr byte, paramNumber byte, data []byte) []byte {
	payload := make([]byte, 0, 1+len(data))
	payload = append(payload, paramNumber)
	payload = append(payload, data...)
	return BuildFrame(addr, codeWriteParameter, payload)
}

// BuildReadCurrentTime builds a request to read the device's current
// internal time (section 4.2, param 0x00). Golden vector (addr 0x80):
//   80 04 00 72 E8
func BuildReadCurrentTime(addr byte) []byte {
	return BuildReadParameter(addr, paramReadCurrentTime)
}

// ParseCurrentTimeResponse decodes the 8-byte BCD response to
// BuildReadCurrentTime: sec, min, hour, day-of-week, day-of-month, month,
// year(2-digit, +2000), dst flag (1=winter, 0=summer), per table 4.2.
//
// dow's numeric convention is passed through as-is (0-7 per the device's
// own day-of-week/holiday encoding) rather than reinterpreted here, since
// this package has no reason to assume a particular calendar convention
// beyond what the device itself defines.
func ParseCurrentTimeResponse(frame []byte) (t time.Time, dow int, isWinter bool, err error) {
	_, data, err := ParseResponse(frame)
	if err != nil {
		return time.Time{}, 0, false, err
	}
	if len(data) == 1 {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: device returned status %s instead of time data", ParseStatus(data[0]))
	}
	if len(data) != 8 {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: expected 8-byte time response, got %d bytes", len(data))
	}

	sec, err := codec.DecodeBCDByte(data[0])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: seconds: %w", err)
	}
	min, err := codec.DecodeBCDByte(data[1])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: minutes: %w", err)
	}
	hour, err := codec.DecodeBCDByte(data[2])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: hours: %w", err)
	}
	dowVal, err := codec.DecodeBCDByte(data[3])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: day-of-week: %w", err)
	}
	day, err := codec.DecodeBCDByte(data[4])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: day: %w", err)
	}
	month, err := codec.DecodeBCDByte(data[5])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: month: %w", err)
	}
	year, err := codec.DecodeBCDByte(data[6])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("merkuriy: year: %w", err)
	}

	t = time.Date(2000+year, time.Month(month), day, hour, min, sec, 0, time.UTC)
	return t, dowVal, data[7] == 1, nil
}

// BuildSetTime builds a request to set the device's full internal
// time+date (section 3.10, param 0x0C). dow is passed through using the
// device's own day-of-week convention (not derived from t, since the
// device's numbering is not confirmed to match Go's time.Weekday - the
// caller must supply it explicitly rather than have this function guess).
// isWinter selects the summer/winter flag (section 3.10: "1 зима, 0 лето").
//
// Golden vector (section 3.10's worked example: addr 0x80, 10:55:00
// среда 05 марта 2008 года, zima, dow=3):
//   80 03 0C 00 55 10 03 05 03 08 01 7F 70
func BuildSetTime(addr byte, t time.Time, dow int, isWinter bool) ([]byte, error) {
	data, err := encodeTimeFields(t, dow, isWinter)
	if err != nil {
		return nil, err
	}
	return BuildWriteParameter(addr, paramSetTime, data), nil
}

func encodeTimeFields(t time.Time, dow int, isWinter bool) ([]byte, error) {
	sec, err := codec.EncodeBCDByte(t.Second())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: seconds: %w", err)
	}
	min, err := codec.EncodeBCDByte(t.Minute())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: minutes: %w", err)
	}
	hour, err := codec.EncodeBCDByte(t.Hour())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: hours: %w", err)
	}
	dowByte, err := codec.EncodeBCDByte(dow)
	if err != nil {
		return nil, fmt.Errorf("merkuriy: day-of-week: %w", err)
	}
	day, err := codec.EncodeBCDByte(t.Day())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: day: %w", err)
	}
	month, err := codec.EncodeBCDByte(int(t.Month()))
	if err != nil {
		return nil, fmt.Errorf("merkuriy: month: %w", err)
	}
	year, err := codec.EncodeBCDByte(t.Year() % 100)
	if err != nil {
		return nil, fmt.Errorf("merkuriy: year: %w", err)
	}
	dst := byte(0)
	if isWinter {
		dst = 1
	}
	return []byte{sec, min, hour, dowByte, day, month, year, dst}, nil
}

// BuildCorrectTime builds a request to correct the device's internal time
// within +/-4 minutes/day (section 3.11, param 0x0D) - only sec/min/hour
// are sent, no date/dow/dst. This is the lower-risk operation the manual
// itself distinguishes from a full BuildSetTime (access level 1 or 2,
// versus level 2 only for a full set).
//
// Golden vector (section 3.11's worked example: addr 0x80, correct to
// 10:55:30):
//   80 03 0D 30 55 10 67 E4
func BuildCorrectTime(addr byte, t time.Time) ([]byte, error) {
	sec, err := codec.EncodeBCDByte(t.Second())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: seconds: %w", err)
	}
	min, err := codec.EncodeBCDByte(t.Minute())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: minutes: %w", err)
	}
	hour, err := codec.EncodeBCDByte(t.Hour())
	if err != nil {
		return nil, fmt.Errorf("merkuriy: hours: %w", err)
	}
	return BuildWriteParameter(addr, paramCorrectTime, []byte{sec, min, hour}), nil
}