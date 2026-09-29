package tls

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math/big"
)

// Captured from stable Chrome 152.0.7977.64 on Android. Chromium randomizes
// these Chrome Root Store IDs once per process, which we mirror at package init.
const chrome152TrustAnchorsCapture = "00b80582df13020108839a648c9b2d010c08839a648c9b2d010704d679090c08839a648c9b2d010a04d679090b08839a648c9b2d010d0582df13020e08839a648c9b2d010b04d67909050582df13020d0582df13021404d679090404d679090804d679090d04d679090a04d679090708839a648c9b2d011204d67909010582df13020608839a648c9b2d01080582df13021208839a648c9b2d011304d679090f0582df13021308839a648c9b2d01090582df13020f04d6790906"

var chrome152TrustAnchors = buildChrome152TrustAnchors()

func buildChrome152TrustAnchors() []byte {
	payload, err := hex.DecodeString(chrome152TrustAnchorsCapture)
	if err != nil || len(payload) < 2 || int(binary.BigEndian.Uint16(payload[:2])) != len(payload)-2 {
		panic("utls: invalid embedded Chrome 152 trust anchors")
	}
	list := payload[2:]
	records := make([][]byte, 0, 28)
	for offset := 0; offset < len(list); {
		end := offset + 1 + int(list[offset])
		if end > len(list) {
			panic("utls: malformed embedded Chrome 152 trust anchor")
		}
		records = append(records, append([]byte(nil), list[offset:end]...))
		offset = end
	}
	for i := len(records) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			panic(err)
		}
		records[i], records[j.Int64()] = records[j.Int64()], records[i]
	}
	result := make([]byte, 2, len(payload))
	for _, record := range records {
		result = append(result, record...)
	}
	binary.BigEndian.PutUint16(result[:2], uint16(len(result)-2))
	return result
}

// Chrome 152 GREASEs signature_algorithms. uTLS does not replace the GREASE
// placeholder in that extension yet, so create the wire value per ClientHello.
func chrome152GREASESignatureScheme() SignatureScheme {
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	value := binary.LittleEndian.Uint16(seed[:])
	value = (value & 0xf0) | 0x0a
	value |= value << 8
	return SignatureScheme(value)
}
