package demand

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// ChecksumOf returns a stable checksum over the measurement fields.
// Producers set Sample.Checksum to this value; tampered/corrupted frames
// then fail validation with RejectChecksum.
func ChecksumOf(s Sample) string {
	var b strings.Builder
	b.WriteString(string(s.Source))
	b.WriteByte('|')
	b.WriteString(s.DeviceID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(s.At.UnixNano(), 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatFloat(s.PowerKW, 'f', 4, 64))
	b.WriteByte('|')
	b.WriteString(strconv.FormatFloat(s.SOC, 'f', 4, 64))
	b.WriteByte('|')
	b.WriteString(strconv.FormatFloat(s.EnergyWh, 'f', 4, 64))
	b.WriteByte('|')
	b.WriteString(strconv.FormatFloat(s.TempC, 'f', 4, 64))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}
