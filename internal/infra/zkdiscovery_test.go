package infra

import (
	"encoding/binary"
	"testing"
)

// encodeNameNodeContent builds the protobuf registration payload the UCloud
// services publish: field 1 (string) = ip, field 10 (varint) = port.
func encodeNameNodeContent(ip string, port int) []byte {
	var b []byte
	// field 1, wire type 2 (length-delimited)
	b = append(b, 0x0a)
	b = append(b, byte(len(ip)))
	b = append(b, ip...)
	// field 10, wire type 0 (varint): tag = 10<<3 | 0 = 80 → two bytes 0x50 0x01
	var tag [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tag[:], uint64(10<<3))
	b = append(b, tag[:n]...)
	var v [binary.MaxVarintLen64]byte
	n = binary.PutUvarint(v[:], uint64(port))
	b = append(b, v[:n]...)
	return b
}

func TestDecodeNameNodeContentRoundTrip(t *testing.T) {
	raw := encodeNameNodeContent("10.72.136.45", 21080)
	ep, err := decodeNameNodeContent(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.IP != "10.72.136.45" || ep.Port != 21080 {
		t.Fatalf("got %+v", ep)
	}
	if got := ep.Addr(); got != "10.72.136.45:21080" {
		t.Fatalf("addr = %q", got)
	}
	if got := ep.URL(); got != "http://10.72.136.45:21080" {
		t.Fatalf("url = %q", got)
	}
}

func TestDecodeNameNodeContentMultiByteVarint(t *testing.T) {
	// Port 20141 needs a 3-byte varint; IP length 13 is a single byte.
	raw := encodeNameNodeContent("10.72.164.202", 20141)
	ep, err := decodeNameNodeContent(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.Port != 20141 {
		t.Fatalf("port = %d", ep.Port)
	}
}

func TestDecodeRegistrationFormatSniffing(t *testing.T) {
	// JSON payload is sniffed by a leading "{".
	ep, err := decodeRegistration([]byte(`{"ip":"1.2.3.4","port":9999}`), "")
	if err != nil || ep.IP != "1.2.3.4" || ep.Port != 9999 {
		t.Fatalf("json sniff: %+v %v", ep, err)
	}
	// Binary payload is sniffed as protobuf.
	ep, err = decodeRegistration(encodeNameNodeContent("5.6.7.8", 1234), "")
	if err != nil || ep.IP != "5.6.7.8" {
		t.Fatalf("protobuf sniff: %+v %v", ep, err)
	}
}

func TestDecodeRegistrationRejectsIncomplete(t *testing.T) {
	if _, err := decodeRegistration([]byte("not-a-registration"), ""); err == nil {
		t.Fatal("expected error for malformed payload")
	}
	if _, err := decodeNameNodeContent(nil); err == nil {
		t.Fatal("expected error for empty payload")
	}
}
