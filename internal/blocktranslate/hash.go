package blocktranslate

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"sort"
)

// StateHash is FNV-1a over sorted, typed little-endian NBT name/states.
// Matches the Bedrock hashed network palette, including minecraft:unknown.
func StateHash(name string, properties map[string]any) (uint32, error) {
	if name == "minecraft:unknown" {
		return 0xfffffffe, nil
	}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var data bytes.Buffer
	data.Write([]byte{10, 0, 0}) // unnamed root compound
	writeNBTString := func(value string) error {
		if len(value) > 0xffff {
			return fmt.Errorf("NBT string too long")
		}
		_ = binary.Write(&data, binary.LittleEndian, uint16(len(value)))
		data.WriteString(value)
		return nil
	}
	data.WriteByte(8)
	if err := writeNBTString("name"); err != nil {
		return 0, err
	}
	if err := writeNBTString(name); err != nil {
		return 0, err
	}
	data.WriteByte(10)
	if err := writeNBTString("states"); err != nil {
		return 0, err
	}
	for _, key := range keys {
		value := properties[key]
		switch v := value.(type) {
		case string:
			data.WriteByte(8)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			if err := writeNBTString(v); err != nil {
				return 0, err
			}
		case bool:
			data.WriteByte(1)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			if v {
				data.WriteByte(1)
			} else {
				data.WriteByte(0)
			}
		case int8:
			data.WriteByte(1)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			data.WriteByte(byte(v))
		case uint8:
			data.WriteByte(1)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			data.WriteByte(v)
		case int16:
			data.WriteByte(2)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			_ = binary.Write(&data, binary.LittleEndian, v)
		case uint16:
			data.WriteByte(2)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			_ = binary.Write(&data, binary.LittleEndian, v)
		case int32:
			data.WriteByte(3)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			_ = binary.Write(&data, binary.LittleEndian, v)
		case uint32:
			data.WriteByte(3)
			if err := writeNBTString(key); err != nil {
				return 0, err
			}
			_ = binary.Write(&data, binary.LittleEndian, v)
		default:
			return 0, fmt.Errorf("unsupported state value %s: %T", key, value)
		}
	}
	data.Write([]byte{0, 0}) // end states, end root
	h := fnv.New32a()
	_, _ = h.Write(data.Bytes())
	return h.Sum32(), nil
}
