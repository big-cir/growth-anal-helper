package cdc

import (
	"encoding/json"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Pos is the position run stores: the end of the last finished source transaction,
// and the start and checksum of the event that ended it, to recognise the same binlog on return.
type Pos struct {
	File       string `json:"file"`
	Pos        uint32 `json:"pos"`
	EventStart uint32 `json:"event_start,omitempty"`
	EventCRC   uint32 `json:"event_crc,omitempty"`
}

// Position is the binlog position alone.
func (p Pos) Position() mysql.Position { return mysql.Position{Name: p.File, Pos: p.Pos} }

func (p Pos) String() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// ParsePos reads a stored position.
func ParsePos(s string) (Pos, error) {
	var p Pos
	if err := json.Unmarshal([]byte(s), &p); err != nil || p.File == "" || p.Pos < 4 {
		return p, &StopError{Msg: "the stored position is not understood"}
	}
	return p, nil
}
