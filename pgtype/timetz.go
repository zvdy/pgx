package pgtype

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/internal/pgio"
)

const (
	pgTimetzHourFormat       = "15:04:05Z07"
	pgTimetzMinuteFormat     = "15:04:05Z07:00"
	pgTimetzSecondFormat     = "15:04:05Z07:00:00"
	pgTimetzHourFormatFrac   = "15:04:05.999999Z07"
	pgTimetzMinuteFormatFrac = "15:04:05.999999Z07:00"
	pgTimetzSecondFormatFrac = "15:04:05.999999Z07:00:00"
)

type TimetzScanner interface {
	ScanTimetz(v Timetz) error
}

type TimetzValuer interface {
	TimetzValue() (Timetz, error)
}

// Timetz represents the PostgreSQL timetz (time with time zone) type.
type Timetz struct {
	Time  time.Time
	Valid bool
}

// ScanTimetz implements the [TimetzScanner] interface.
func (tz *Timetz) ScanTimetz(v Timetz) error {
	*tz = v
	return nil
}

// TimetzValue implements the [TimetzValuer] interface.
func (tz Timetz) TimetzValue() (Timetz, error) {
	return tz, nil
}

// Scan implements the [database/sql.Scanner] interface.
func (tz *Timetz) Scan(src any) error {
	if src == nil {
		*tz = Timetz{}
		return nil
	}

	switch src := src.(type) {
	case string:
		return (&scanPlanTextTimetzToTimetzScanner{}).Scan([]byte(src), tz)
	case time.Time:
		*tz = Timetz{Time: src, Valid: true}
		return nil
	}

	return fmt.Errorf("cannot scan %T", src)
}

// Value implements the [database/sql/driver.Valuer] interface.
func (tz Timetz) Value() (driver.Value, error) {
	if !tz.Valid {
		return nil, nil
	}
	return tz.Time, nil
}

type TimetzCodec struct{}

func (TimetzCodec) FormatSupported(format int16) bool {
	return format == TextFormatCode || format == BinaryFormatCode
}

func (TimetzCodec) PreferredFormat() int16 {
	return BinaryFormatCode
}

func (TimetzCodec) PlanEncode(m *Map, oid uint32, format int16, value any) EncodePlan {
	if _, ok := value.(TimetzValuer); !ok {
		return nil
	}

	switch format {
	case BinaryFormatCode:
		return encodePlanTimetzCodecBinary{}
	case TextFormatCode:
		return encodePlanTimetzCodecText{}
	}

	return nil
}

type encodePlanTimetzCodecBinary struct{}

func (encodePlanTimetzCodecBinary) Encode(value any, buf []byte) (newBuf []byte, err error) {
	tz, err := value.(TimetzValuer).TimetzValue()
	if err != nil {
		return nil, err
	}

	if !tz.Valid {
		return nil, nil
	}

	t := tz.Time
	usec := int64(t.Hour())*microsecondsPerHour +
		int64(t.Minute())*microsecondsPerMinute +
		int64(t.Second())*microsecondsPerSecond +
		int64(t.Nanosecond())/1000

	_, goOffset := t.Zone()
	// PostgreSQL stores timezone as seconds west of UTC; Go uses seconds east — negate.
	pgOffset := int32(-goOffset)

	buf = pgio.AppendInt64(buf, usec)
	buf = pgio.AppendInt32(buf, pgOffset)

	return buf, nil
}

type encodePlanTimetzCodecText struct{}

func (encodePlanTimetzCodecText) Encode(value any, buf []byte) (newBuf []byte, err error) {
	tz, err := value.(TimetzValuer).TimetzValue()
	if err != nil {
		return nil, err
	}

	if !tz.Valid {
		return nil, nil
	}

	s := tz.Time.Truncate(time.Microsecond).Format(pgTimetzSecondFormatFrac)
	buf = append(buf, s...)

	return buf, nil
}

func (TimetzCodec) PlanScan(m *Map, oid uint32, format int16, target any) ScanPlan {
	switch format {
	case BinaryFormatCode:
		switch target.(type) {
		case TimetzScanner:
			return &scanPlanBinaryTimetzToTimetzScanner{}
		}
	case TextFormatCode:
		switch target.(type) {
		case TimetzScanner:
			return &scanPlanTextTimetzToTimetzScanner{}
		}
	}

	return nil
}

type scanPlanBinaryTimetzToTimetzScanner struct{}

func (plan *scanPlanBinaryTimetzToTimetzScanner) Scan(src []byte, dst any) error {
	scanner := (dst).(TimetzScanner)

	if src == nil {
		return scanner.ScanTimetz(Timetz{})
	}

	if len(src) != 12 {
		return fmt.Errorf("invalid length for timetz: %v", len(src))
	}

	usec := int64(binary.BigEndian.Uint64(src[:8]))
	pgOffset := int32(binary.BigEndian.Uint32(src[8:12]))
	goOffset := -int(pgOffset)

	loc := time.FixedZone("", goOffset)

	hours := usec / microsecondsPerHour
	usec -= hours * microsecondsPerHour
	minutes := usec / microsecondsPerMinute
	usec -= minutes * microsecondsPerMinute
	seconds := usec / microsecondsPerSecond
	usec -= seconds * microsecondsPerSecond
	ns := int(usec) * 1000

	t := time.Date(2000, 1, 1, int(hours), int(minutes), int(seconds), ns, loc)

	return scanner.ScanTimetz(Timetz{Time: t, Valid: true})
}

type scanPlanTextTimetzToTimetzScanner struct{}

func (plan *scanPlanTextTimetzToTimetzScanner) Scan(src []byte, dst any) error {
	scanner := (dst).(TimetzScanner)

	if src == nil {
		return scanner.ScanTimetz(Timetz{})
	}

	s := string(src)

	var format string
	if len(s) > 8 && s[8] == '.' {
		tzIdx := -1
		for i := 9; i < len(s); i++ {
			if s[i] == '+' || s[i] == '-' {
				tzIdx = i
				break
			}
		}
		if tzIdx == -1 {
			format = pgTimetzHourFormatFrac
		} else {
			tzPart := s[tzIdx:]
			colonCount := 0
			for _, c := range tzPart {
				if c == ':' {
					colonCount++
				}
			}
			switch colonCount {
			case 2:
				format = pgTimetzSecondFormatFrac
			case 1:
				format = pgTimetzMinuteFormatFrac
			default:
				format = pgTimetzHourFormatFrac
			}
		}
	} else {
		tzIdx := -1
		for i := 8; i < len(s); i++ {
			if s[i] == '+' || s[i] == '-' || s[i] == 'Z' {
				tzIdx = i
				break
			}
		}
		if tzIdx == -1 {
			format = pgTimetzHourFormat
		} else {
			tzPart := s[tzIdx:]
			colonCount := 0
			for _, c := range tzPart {
				if c == ':' {
					colonCount++
				}
			}
			switch colonCount {
			case 2:
				format = pgTimetzSecondFormat
			case 1:
				format = pgTimetzMinuteFormat
			default:
				format = pgTimetzHourFormat
			}
		}
	}

	t, err := time.Parse(format, s)
	if err != nil {
		return fmt.Errorf("cannot parse %q as timetz: %w", s, err)
	}

	return scanner.ScanTimetz(Timetz{Time: t, Valid: true})
}

func (c TimetzCodec) DecodeDatabaseSQLValue(m *Map, oid uint32, format int16, src []byte) (driver.Value, error) {
	if src == nil {
		return nil, nil
	}

	var tz Timetz
	if err := codecScan(c, m, oid, format, src, &tz); err != nil {
		return nil, err
	}

	return tz.Time, nil
}

func (c TimetzCodec) DecodeValue(m *Map, oid uint32, format int16, src []byte) (any, error) {
	if src == nil {
		return nil, nil
	}

	var tz Timetz
	if err := codecScan(c, m, oid, format, src, &tz); err != nil {
		return nil, err
	}

	return tz.Time, nil
}
