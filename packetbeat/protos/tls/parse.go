package tls

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/common/streambuf"
	"github.com/elastic/beats/libbeat/logp"
)

type direction uint8

const (
	dirUnknown direction = iota
	dirClient
	dirServer
)

const (
	maxTLSRecordLength = (1 << 14) + 2048
	// For safety, ignore handshake messages longer than 64k (same as stdlib)
	maxHandshakeSize    = 1 << 16
	recordHeaderSize    = 5
	handshakeHeaderSize = 4
	helloHeaderLength   = 7
	randomDataLength    = 28
)

type recordType uint8

const (
	recordTypeChangeCipherSpec recordType = 20
	recordTypeAlert                       = 21
	recordTypeHandshake                   = 22
	recordTypeApplicationData             = 23
)

type handshakeType uint8

const (
	helloRequest       handshakeType = 0
	clientHello                      = 1
	serverHello                      = 2
	certificate                      = 11
	serverKeyExchange                = 12
	certificateRequest               = 13
	clientKeyExchange                = 16
)

type parserResult int8

const (
	resultOK parserResult = iota
	resultFailed
	resultMore
	resultEncrypted
)

type tlsTicket struct {
	present bool
	value   string
}

type parser struct {
	// Buffer to accumulate records until a full handshake message
	// is received
	handshakeBuf streambuf.Buffer

	direction    direction
	alerts       []alert
	certificates []*x509.Certificate
	hello        *helloMessage

	// If this end of the connection (server) asked the other end (client)
	// for a certificate
	certRequested bool

	// If a key-exchange message has been sent. Used to detect session resumption
	keyExchanged bool
}

type tlsVersion struct {
	major, minor uint8
}

type recordHeader struct {
	recordType recordType
	version    tlsVersion
	length     uint16
}

type handshakeHeader struct {
	handshakeType handshakeType
	length        int
}

type helloMessage struct {
	version   tlsVersion
	timestamp uint32
	sessionID string
	ticket    tlsTicket
	supported struct {
		cipherSuites []cipherSuite
		compression  []compressionMethod
	}
	selected struct {
		cipherSuite cipherSuite
		compression compressionMethod
	}
	extensions common.MapStr
}

func readRecordHeader(buf *streambuf.Buffer) (*recordHeader, error) {
	var (
		header recordHeader
		err    error
		record uint8
	)
	if record, err = buf.ReadNetUint8At(0); err != nil {
		return nil, err
	}
	header.recordType = recordType(record)
	if header.version.major, err = buf.ReadNetUint8At(1); err != nil {
		return nil, err
	}
	if header.version.minor, err = buf.ReadNetUint8At(2); err != nil {
		return nil, err
	}
	if header.length, err = buf.ReadNetUint16At(3); err != nil {
		return nil, err
	}
	return &header, nil
}

func readHandshakeHeader(buf *streambuf.Buffer) (*handshakeHeader, error) {
	var err error
	var len8, typ uint8
	var len16 uint16
	if typ, err = buf.ReadNetUint8At(0); err != nil {
		return nil, err
	}
	if len8, err = buf.ReadNetUint8At(1); err != nil {
		return nil, err
	}
	if len16, err = buf.ReadNetUint16At(2); err != nil {
		return nil, err
	}
	return &handshakeHeader{handshakeType(typ),
		int(len16) | (int(len8) << 16)}, nil
}

func (header *recordHeader) String() string {
	return fmt.Sprintf("recordHeader type[%v] version[%v] length[%d]",
		header.recordType, header.version, header.length)
}

func (header *recordHeader) isValid() bool {
	return header.version.major == 3 && header.length <= maxTLSRecordLength
}

func (hello helloMessage) toMap() common.MapStr {
	m := common.MapStr{
		"version":   fmt.Sprintf("%d.%d", hello.version.major, hello.version.minor),
		"timestamp": time.Unix(int64(hello.timestamp), 0).UTC(),
	}
	if len(hello.sessionID) != 0 {
		m["session_id"] = hello.sessionID
	}

	if len(hello.supported.cipherSuites) > 0 || len(hello.supported.compression) > 0 {
		ciphers := make([]string, len(hello.supported.cipherSuites))
		for idx, code := range hello.supported.cipherSuites {
			ciphers[idx] = code.String()
		}
		m["supported_ciphers"] = ciphers

		comp := make([]string, len(hello.supported.compression))
		for idx, code := range hello.supported.compression {
			comp[idx] = code.String()
		}
		m["supported_compression_methods"] = comp
	} else {
		m["selected_cipher"] = hello.selected.cipherSuite.String()
		m["selected_compression_method"] = hello.selected.compression.String()
	}

	if hello.extensions != nil {
		m["extensions"] = hello.extensions
	}
	return m
}

func (parser *parser) parse(buf *streambuf.Buffer) parserResult {

	for buf.Avail(recordHeaderSize) {

		header, err := readRecordHeader(buf)
		if err != nil || !header.isValid() {
			if err != nil {
				logp.Warn("internal buffer error: %v", err)
			}
			return resultFailed
		}

		limit := recordHeaderSize + int(header.length)
		if !buf.Avail(limit) {
			// wait for complete record
			return resultMore
		}

		switch header.recordType {
		case recordTypeChangeCipherSpec: // single message of size 1 (byte 1)
			if isDebug {
				debugf("handshake completed")
			}
			// discard remaining data for this stream (encrypted)
			buf.Advance(buf.Len())
			return resultEncrypted

		case recordTypeHandshake:
			if isDebug {
				debugf("got handshake record of size %d", header.length)
			}
			if err = parser.bufferHandshake(buf, int(header.length)); err != nil {
				logp.Warn("Error parsing handshake message: %v", err)
				return resultFailed
			}

		case recordTypeAlert:
			if err = parser.parseAlert(newBufferView(buf, recordHeaderSize, int(header.length))); err != nil {
				logp.Warn("Error parsing alert message: %v", err)
				return resultFailed
			}

		case recordTypeApplicationData:
			// TODO: Request / Response analytics
			if isDebug {
				debugf("ignoring application data length %d", header.length)
			}

		default:
			if isDebug {
				debugf("ignoring record type %d length %d", header.recordType, header.length)
			}
		}

		buf.Advance(limit)
	}

	if buf.Len() == 0 {
		return resultOK
	}
	return resultMore
}

func (parser *parser) bufferHandshake(buf *streambuf.Buffer, length int) error {
	// TODO: parse in-place if message in received buffer is complete
	if err := parser.handshakeBuf.Append(buf.Bytes()[recordHeaderSize : recordHeaderSize+length]); err != nil {
		logp.Warn("failed appending to buffer: %v", err)
		// Discard buffer
		parser.handshakeBuf.Init(nil, false)
		return err
	}
	for parser.handshakeBuf.Avail(handshakeHeaderSize) {
		// type
		header, err := readHandshakeHeader(&parser.handshakeBuf)
		if err != nil {
			logp.Warn("read failed: %v", err)
			parser.handshakeBuf.Init(nil, false)
			return err
		}
		if header.length > maxHandshakeSize {
			// Discard buffer
			parser.handshakeBuf.Init(nil, false)
			return fmt.Errorf("message too large (%d bytes)", header.length)
		}
		limit := handshakeHeaderSize + header.length
		if limit > parser.handshakeBuf.Len() {
			break
		}
		if !parser.parseHandshake(header.handshakeType,
			bufferView{&parser.handshakeBuf, handshakeHeaderSize, limit}) {
			parser.handshakeBuf.Advance(limit)
			return fmt.Errorf("bad handshake %+v", header)
		}
		parser.handshakeBuf.Advance(limit)
	}
	if parser.handshakeBuf.Len() == 0 {
		parser.handshakeBuf.Reset()
	}
	return nil
}

func (parser *parser) setDirection(dir direction) {
	if parser.direction != dir && parser.direction != dirUnknown {
		logp.Warn("client/server identification mismatch")
	}
	parser.direction = dir
}

func (parser *parser) parseHandshake(handshakeType handshakeType, buffer bufferView) bool {
	if isDebug {
		debugf("got handshake message %v [%d]", handshakeType, buffer.length())
	}
	switch handshakeType {
	case helloRequest:
		parser.setDirection(dirServer)
		return parseHelloRequest(buffer)

	case clientHello:
		parser.setDirection(dirClient)
		if parser.hello = parseClientHello(buffer); parser.hello == nil {
			return false
		}
		return true

	case serverHello:
		parser.setDirection(dirServer)
		if parser.hello = parseServerHello(buffer); parser.hello == nil {
			return false
		}
		return true

	case certificate:
		certs := parseCertificates(buffer)
		parser.certificates = append(parser.certificates, certs...)

	case certificateRequest:
		parser.setDirection(dirServer)
		parser.certRequested = true

	case clientKeyExchange:
		parser.setDirection(dirClient)
		parser.keyExchanged = true

	case serverKeyExchange:
		parser.setDirection(dirServer)
		parser.keyExchanged = true
	}
	return true
}

// "{\"dst\":{\"IP\":\"192.168.0.2\",\"Port\":27017,\"Name\":\"\",\"Cmdline\":\"\",\"Proc\":\"\"},\"src\":{\"IP\":\"192.168.0.1\",\"Port\":6512,\"Name\":\"\",\"Cmdline\":\"\",\"Proc\":\"\"},\"status\":\"Error\",\"tls\":{\"client_certificate_requested\":false,\"client_hello\":{\"ciphers\":[\"(unknown:0x3a3a)\",\"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256\",\"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256\",\"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384\",\"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384\",\"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256\",\"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256\",\"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA\",\"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA\",\"TLS_RSA_WITH_AES_128_GCM_SHA256\",\"TLS_RSA_WITH_AES_256_GCM_SHA384\",\"TLS_RSA_WITH_AES_128_CBC_SHA\",\"TLS_RSA_WITH_AES_256_CBC_SHA\",\"TLS_RSA_WITH_3DES_EDE_CBC_SHA\"],\"compression_methods\":[\"null\"],\"extensions\":{\"_unparsed_\":\"56026,renegotiation_info,23,status_request,18,16,30032,43690\",\"ec_points_formats\":\"uncompressed\",\"server_name_indication\":\"example.org\",\"session_ticket\":\"\",\"signature_algorithms\":\"ecdsa_secp256r1_sha256,rsa_pss_sha256,rsa_pkcs1_sha256,ecdsa_secp384r1_sha384,rsa_pss_sha384,rsa_pkcs1_sha384,rsa_pss_sha512,rsa_pkcs1_sha512,rsa_pkcs1_sha1\",\"supported_groups\":\"(unknown:0x6a6a),x25519,secp256r1,secp384r1\"},\"timestamp\":862445486,\"version\":\"3.3\"},\"handshake_completed\":false,\"resumed\":false},\"type\":\"tls\"}" (expected)
// "{\"dst\":{\"IP\":\"192.168.0.2\",\"Port\":27017,\"Name\":\"\",\"Cmdline\":\"\",\"Proc\":\"\"},\"src\":{\"IP\":\"192.168.0.1\",\"Port\":6512,\"Name\":\"\",\"Cmdline\":\"\",\"Proc\":\"\"},\"status\":\"Error\",\"tls\":{\"client_certificate_requested\":false,\"client_hello\":{\"ciphers\":[\"(unknown:0x3a3a)\",\"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256\",\"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256\",\"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384\",\"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384\",\"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256\",\"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256\",\"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA\",\"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA\",\"TLS_RSA_WITH_AES_128_GCM_SHA256\",\"TLS_RSA_WITH_AES_256_GCM_SHA384\",\"TLS_RSA_WITH_AES_128_CBC_SHA\",\"TLS_RSA_WITH_AES_256_CBC_SHA\",\"TLS_RSA_WITH_3DES_EDE_CBC_SHA\"],\"compression_method\":\"null\",\"extensions\":{\"_unparsed_\":\"56026,renegotiation_info,23,status_request,18,16,30032,43690\",\"ec_points_formats\":\"uncompressed\",\"server_name_indication\":\"example.org\",\"session_ticket\":\"\",\"signature_algorithms\":\"ecdsa_secp256r1_sha256,rsa_pss_sha256,rsa_pkcs1_sha256,ecdsa_secp384r1_sha384,rsa_pss_sha384,rsa_pkcs1_sha384,rsa_pss_sha512,rsa_pkcs1_sha512,rsa_pkcs1_sha1\",\"supported_groups\":\"(unknown:0x6a6a),x25519,secp256r1,secp384r1\"},\"timestamp\":862445486,\"version\":\"3.3\"},\"handshake_completed\":false,\"resumed\":false},\"type\":\"tls\"}" (actual)
func parseHelloRequest(buffer bufferView) bool {
	if buffer.length() != 0 {
		logp.Warn("non-empty hello request")
	}
	return true
}

func parseCommonHello(buffer bufferView, dest *helloMessage) (int, bool) {
	var sessionIDLength uint8

	if !buffer.read8(0, &dest.version.major) ||
		!buffer.read8(1, &dest.version.minor) ||
		!buffer.read32Net(2, &dest.timestamp) ||
		// ignore 28 random bytes
		!buffer.read8(6+randomDataLength, &sessionIDLength) {
		logp.Warn("failed reading hello message")
		return 0, false
	}

	if dest.version.major != 3 {
		logp.Warn("Not a TLS hello (reported version %d.%d)",
			dest.version.major, dest.version.minor)
		return 0, false
	}
	if sessionIDLength > 32 {
		logp.Warn("Not a TLS hello (session id length %d out of bounds)", sessionIDLength)
		return 0, false
	}

	if bytes := buffer.readBytes(7+randomDataLength, int(sessionIDLength)); len(bytes) == int(sessionIDLength) {
		dest.sessionID = hex.EncodeToString(bytes)
	} else {
		logp.Warn("Not a TLS hello (failed reading session ID)")
		return 0, false
	}
	return helloHeaderLength + randomDataLength + int(sessionIDLength), true
}

func (hello *helloMessage) parseExtensions(buffer bufferView) {
	hello.extensions = parseExtensions(buffer)
	if ticket, err := hello.extensions.GetValue("session_ticket"); err == nil {
		if value, ok := ticket.(string); ok {
			hello.ticket.present = true
			hello.ticket.value = value
		} else {
			logp.Err("tls ticket data type error")
		}
	}
}

func parseClientHello(buffer bufferView) *helloMessage {
	var result helloMessage
	pos, ok := parseCommonHello(buffer, &result)
	if !ok {
		return nil
	}

	var cipherSuitesLength uint16
	if !buffer.read16Net(pos, &cipherSuitesLength) {
		logp.Warn("failed parsing client hello cipher suite length")
		return nil
	}

	for base := pos + 2; base < pos+2+int(cipherSuitesLength); base += 2 {
		var cipher uint16
		if !buffer.read16Net(base, &cipher) {
			logp.Warn("failed parsing client hello cipher suite")
			return nil
		}
		result.supported.cipherSuites = append(result.supported.cipherSuites, cipherSuite(cipher))
	}

	pos += 2 + int(cipherSuitesLength)
	var compMethodsLength uint8
	if !buffer.read8(pos, &compMethodsLength) {
		logp.Warn("failed parsing client hello compression methods length")
		return nil
	}
	limit := pos + 1 + int(compMethodsLength)
	for base := pos + 1; base < limit; base++ {
		var method uint8
		if !buffer.read8(base, &method) {
			logp.Warn("failed parsing client hello compression methods")
			return nil
		}
		result.supported.compression = append(result.supported.compression, compressionMethod(method))
	}

	result.parseExtensions(buffer.subview(limit, buffer.limit-limit))
	return &result
}

func parseServerHello(buffer bufferView) *helloMessage {
	var result helloMessage
	pos, ok := parseCommonHello(buffer, &result)
	if !ok {
		return nil
	}

	var cipher uint16
	var compression uint8
	if !buffer.read16Net(pos, &cipher) ||
		!buffer.read8(pos+2, &compression) {
		return nil
	}
	result.selected.cipherSuite = cipherSuite(cipher)
	result.selected.compression = compressionMethod(compression)
	result.parseExtensions(buffer.subview(pos+3, buffer.limit-pos-3))
	return &result
}

func parseCertificates(buffer bufferView) []*x509.Certificate {
	var totalLen uint32
	if !buffer.read24Net(0, &totalLen) || int(totalLen+3) != buffer.length() {
		return nil
	}

	var certs []*x509.Certificate

	for pos, limit := 3, int(totalLen)+3; pos+3 <= limit; {
		var certLen uint32
		if !buffer.read24Net(pos, &certLen) || pos+3+int(certLen) > limit {
			return nil
		}
		cert := buffer.readBytes(pos+3, int(certLen))
		if len(cert) != int(certLen) {
			return nil
		}
		parsed, err := x509.ParseCertificate(cert)
		if err != nil {
			return nil
		}
		certs = append(certs, parsed)
		pos += 3 + int(certLen)
	}
	return certs
}

func (version tlsVersion) String() string {
	return fmt.Sprintf("%d.%d", version.major, version.minor)
}

func certToMap(cert *x509.Certificate) common.MapStr {
	certMap := common.MapStr{
		"signature_algorithm":  cert.SignatureAlgorithm.String(),
		"public_key_algorithm": toString(cert.PublicKeyAlgorithm),
		"version":              cert.Version,
		"serial_number":        cert.SerialNumber.Text(10),
		"issuer":               toMap(&cert.Issuer),
		"subject":              toMap(&cert.Subject),
		"not_before":           cert.NotBefore,
		"not_after":            cert.NotAfter,
	}
	san := make([]string, 0, len(cert.DNSNames)+len(cert.IPAddresses)+len(cert.EmailAddresses))
	san = append(append(san, cert.DNSNames...), cert.EmailAddresses...)
	for _, ip := range cert.IPAddresses {
		san = append(san, ip.String())
	}
	if len(san) > 0 {
		certMap["alternative_names"] = san
	}
	return certMap
}

func toMap(name *pkix.Name) common.MapStr {
	result := common.MapStr{}
	fields := []struct {
		name  string
		value interface{}
	}{
		{"country", name.Country},
		{"organization", name.Organization},
		{"organizational_unit", name.OrganizationalUnit},
		{"locality", name.Locality},
		{"province", name.Province},
		{"postal_code", name.PostalCode},
		{"serial_number", name.SerialNumber},
		{"common_name", name.CommonName},
		{"street_address", name.StreetAddress},
	}
	for _, field := range fields {
		var str string
		switch value := field.value.(type) {
		case string:
			str = value
		case []string:
			str = strings.Join(value, " ")
		}
		if len(str) > 0 {
			result[field.name] = str
		}
	}
	return result
}

func (parser *parser) hasInfo() bool {
	return parser.hello != nil || len(parser.alerts) != 0 || len(parser.certificates) != 0
}
