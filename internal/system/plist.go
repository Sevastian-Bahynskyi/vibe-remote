package system

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// decodePlist reads an Apple XML property list into plain Go values. IOKit
// embeds <data> nodes that `plutil -convert json` refuses to translate, so the
// battery reader parses the plist directly instead of shelling out twice.
func decodePlist(reader io.Reader) (any, error) {
	decoder := xml.NewDecoder(reader)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "plist" {
			continue
		}
		return nextPlistValue(decoder)
	}
}

func nextPlistValue(decoder *xml.Decoder) (any, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			return decodePlistElement(decoder, element)
		case xml.EndElement:
			return nil, fmt.Errorf("plist: missing value before </%s>", element.Name.Local)
		}
	}
}

func decodePlistElement(decoder *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict":
		return decodePlistDict(decoder)
	case "array":
		return decodePlistArray(decoder)
	case "true":
		return true, decoder.Skip()
	case "false":
		return false, decoder.Skip()
	case "integer", "real":
		return decodePlistNumber(decoder, start)
	default:
		var text string
		if err := decoder.DecodeElement(&text, &start); err != nil {
			return nil, err
		}
		return text, nil
	}
}

func decodePlistDict(decoder *xml.Decoder) (any, error) {
	result := map[string]any{}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.EndElement:
			return result, nil
		case xml.StartElement:
			if element.Name.Local != "key" {
				return nil, fmt.Errorf("plist: expected <key>, got <%s>", element.Name.Local)
			}
			var key string
			if err := decoder.DecodeElement(&key, &element); err != nil {
				return nil, err
			}
			value, err := nextPlistValue(decoder)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
	}
}

func decodePlistArray(decoder *xml.Decoder) (any, error) {
	result := []any{}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.EndElement:
			return result, nil
		case xml.StartElement:
			value, err := decodePlistElement(decoder, element)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
	}
}

// decodePlistNumber keeps the raw text when a value does not fit its Go type.
// IOKit reports some counters as unsigned 64-bit values that overflow int64;
// widening those to float64 would hand back a garbage integer, so they stay
// strings and callers see them as unreadable rather than wrong.
func decodePlistNumber(decoder *xml.Decoder, start xml.StartElement) (any, error) {
	var text string
	if err := decoder.DecodeElement(&text, &start); err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if start.Name.Local == "real" {
		if value, err := strconv.ParseFloat(text, 64); err == nil {
			return value, nil
		}
		return text, nil
	}
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		return value, nil
	}
	return text, nil
}

func plistDict(value any, key string) map[string]any {
	parent, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	child, _ := parent[key].(map[string]any)
	return child
}

func plistInt(value any, key string) (int64, bool) {
	parent, ok := value.(map[string]any)
	if !ok {
		return 0, false
	}
	switch number := parent[key].(type) {
	case int64:
		return number, true
	case float64:
		return int64(number), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(number), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func plistBool(value any, key string) bool {
	parent, ok := value.(map[string]any)
	if !ok {
		return false
	}
	flag, _ := parent[key].(bool)
	return flag
}
