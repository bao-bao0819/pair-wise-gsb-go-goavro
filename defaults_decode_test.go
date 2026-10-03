package goavro

import "testing"

func TestDecodedDefaultsAreIndependent(t *testing.T) {
	schema := `{"type":"record","name":"r1","fields":[
		{"name":"nums","type":{"type":"array","items":"int"},"default":[1,2,3]},
		{"name":"blob","type":"bytes","default":"abc"}
	]}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := codec.NativeFromTextual([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := codec.NativeFromTextual([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	first.(map[string]interface{})["nums"].([]interface{})[0] = int32(99)
	first.(map[string]interface{})["blob"].([]byte)[0] = 'Z'
	if got := second.(map[string]interface{})["nums"].([]interface{})[0]; got != int32(1) {
		t.Errorf("decoded defaults share slice: %v", got)
	}
	if got := second.(map[string]interface{})["blob"].([]byte)[0]; got != byte('a') {
		t.Errorf("decoded defaults share bytes: %v", got)
	}
	// And a third decode reads the pristine default.
	third, _, err := codec.NativeFromTextual([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := third.(map[string]interface{})["nums"].([]interface{})[0]; got != int32(1) {
		t.Errorf("third decode nums: %v", got)
	}
}
