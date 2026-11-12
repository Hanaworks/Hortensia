package protocol

// StrictJSON parses one complete, bounded JSON value. It rejects duplicate
// decoded keys, invalid Unicode, trailing content and non-safe-integer numbers.
// A successful value is a validated JSON tree, not a validated business DTO:
// callers must still check its top-level type and their own field contract.
// A route or file format with a smaller limit must impose that limit first.
func StrictJSON(raw []byte) (any, error) {
	if len(raw) > maxMessageBytes {
		return nil, errBudget
	}
	return strictJSON(raw)
}

// ValidateConfig accepts the shared Config object independently of a prepare
// envelope. It neither normalizes the input bytes nor grants execution rights.
// Schema validation must precede the shared helper's concrete type assertions.
func (v *Validator) ValidateConfig(raw []byte) error {
	doc, err := StrictJSON(raw)
	if err != nil {
		return err
	}
	if err = v.config.Validate(doc); err != nil {
		return errPayload
	}
	return validateConfigFields(doc.(map[string]any))
}

// encoding/json deliberately replaces unpaired UTF-16 escapes with U+FFFD.
// Check those escapes in the original strings first, without implementing a
// second JSON parser. Ordinary escape and JSON grammar checks remain in the
// decoder. The scan is linear and uses no input-sized auxiliary allocation.
func validJSONUnicodeEscapes(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if !inString {
			if raw[i] == '"' {
				inString = true
			}
			continue
		}
		switch raw[i] {
		case '"':
			inString = false
		case '\\':
			i++
			if i >= len(raw) {
				return false
			}
			if raw[i] != 'u' {
				continue
			}
			value, ok := unicodeEscapeValue(raw, i+1)
			if !ok {
				return false
			}
			i += 4
			switch {
			case value >= 0xd800 && value <= 0xdbff:
				if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return false
				}
				low, ok := unicodeEscapeValue(raw, i+3)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			case value >= 0xdc00 && value <= 0xdfff:
				return false
			}
		}
	}
	return !inString
}

func unicodeEscapeValue(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, b := range raw[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			value |= uint16(b - 'A' + 10)
		default:
			return 0, false
		}
	}
	return value, true
}
