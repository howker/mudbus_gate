package storage

import "time"

type ReadingCurrent struct {
DeviceID      string
PointID       string
Instance      string
Value         any
Unit          string
Quality       string
QualityReason string
Timestamp     time.Time
}
