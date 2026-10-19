package pve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// flexInt decodes a JSON number or a numeric string (PVE uses both).
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := string(bytes.Trim(bytes.TrimSpace(b), `"`))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(n)
		return nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("not a number: %s", b)
	}
	*f = flexInt(fl)
	return nil
}

// flexBool decodes true/false, 0/1, or "0"/"1" (PVE uses all of them).
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*f = false
	case bool:
		*f = flexBool(x)
	case float64:
		*f = x != 0
	case string:
		*f = x != "" && x != "0" && x != "false"
	default:
		return fmt.Errorf("not a boolean: %s", b)
	}
	return nil
}
