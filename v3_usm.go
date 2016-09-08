package gosnmp

// Copyright 2012-2016 The GoSNMP Authors. All rights reserved.  Use of this
// source code is governed by a BSD-style license that can be found in the
// LICENSE file.

// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	//"fmt"
	//"hash"
	"fmt"
	"sync/atomic"
)

// SnmpV3AuthProtocol describes the authentication protocol in use by an authenticated SnmpV3 connection.
type SnmpV3AuthProtocol uint8

// NoAuth, MD5, and SHA are implemented
const (
	NoAuth SnmpV3AuthProtocol = 1
	MD5    SnmpV3AuthProtocol = 2
	SHA    SnmpV3AuthProtocol = 3
)

// SnmpV3PrivProtocol is the privacy protocol in use by an private SnmpV3 connection.
type SnmpV3PrivProtocol uint8

// NoPriv, DES implemented, AES planned
const (
	NoPriv SnmpV3PrivProtocol = 1
	DES    SnmpV3PrivProtocol = 2
	AES    SnmpV3PrivProtocol = 3
)

// UsmSecurityParameters is an implementation of SnmpV3SecurityParameters for the UserSecurityModel
type UsmSecurityParameters struct {
	AuthoritativeEngineID    string
	AuthoritativeEngineBoots uint32
	AuthoritativeEngineTime  uint32
	UserName                 string
	AuthenticationParameters string
	PrivacyParameters        []byte

	AuthenticationProtocol SnmpV3AuthProtocol
	PrivacyProtocol        SnmpV3PrivProtocol

	AuthenticationPassphrase string
	PrivacyPassphrase        string

	localDESSalt uint32
	localAESSalt uint64

	Logger Logger
}

// Copy method for UsmSecurityParameters used to copy a SnmpV3SecurityParameters without knowing it's implementation
func (sp *UsmSecurityParameters) Copy() SnmpV3SecurityParameters {
	return &UsmSecurityParameters{AuthoritativeEngineID: sp.AuthoritativeEngineID,
		AuthoritativeEngineBoots: sp.AuthoritativeEngineBoots,
		AuthoritativeEngineTime:  sp.AuthoritativeEngineTime,
		UserName:                 sp.UserName,
		AuthenticationParameters: sp.AuthenticationParameters,
		PrivacyParameters:        sp.PrivacyParameters,
		AuthenticationProtocol:   sp.AuthenticationProtocol,
		PrivacyProtocol:          sp.PrivacyProtocol,
		AuthenticationPassphrase: sp.AuthenticationPassphrase,
		PrivacyPassphrase:        sp.PrivacyPassphrase,
		localDESSalt:             sp.localDESSalt,
		localAESSalt:             sp.localAESSalt,
		Logger:                   sp.Logger,
	}
}

func (sp *UsmSecurityParameters) validate(flags SnmpV3MsgFlags) error {

	securityLevel := flags & AuthPriv // isolate flags that determine security level

	switch securityLevel {
	case AuthPriv:
		if sp.PrivacyProtocol <= NoPriv {
			return fmt.Errorf("SecurityParameters.PrivacyProtocol is required")
		}
		if sp.PrivacyPassphrase == "" {
			return fmt.Errorf("SecurityParameters.PrivacyPassphrase is required")
		}
		fallthrough
	case AuthNoPriv:
		if sp.AuthenticationProtocol <= NoAuth {
			return fmt.Errorf("SecurityParameters.AuthenticationProtocol is required")
		}
		if sp.AuthenticationPassphrase == "" {
			return fmt.Errorf("SecurityParameters.AuthenticationPassphrase is required")
		}
		fallthrough
	case NoAuthNoPriv:
		if sp.UserName == "" {
			return fmt.Errorf("SecurityParameters.UserName is required")
		}
	default:
		return fmt.Errorf("MsgFlags must be populated with an appropriate security level")
	}

	return nil
}

func (sp *UsmSecurityParameters) init(log Logger) error {
	var err error

	sp.Logger = log

	switch sp.PrivacyProtocol {
	case AES:
		salt := make([]byte, 8)
		_, err = crand.Read(salt)
		if err != nil {
			return fmt.Errorf("Error creating a cryptographically secure salt: %s\n", err.Error())
		}
		sp.localAESSalt = binary.BigEndian.Uint64(salt)
	case DES:
		salt := make([]byte, 4)
		_, err = crand.Read(salt)
		if err != nil {
			return fmt.Errorf("Error creating a cryptographically secure salt: %s\n", err.Error())
		}
		sp.localDESSalt = binary.BigEndian.Uint32(salt)
	}

	return nil
}

func castUsmSecParams(secParams SnmpV3SecurityParameters) (*UsmSecurityParameters, error) {
	s, ok := secParams.(*UsmSecurityParameters)
	if !ok || s == nil {
		return nil, fmt.Errorf("SecurityParameters is not of type *UsmSecurityParameters")
	}
	return s, nil
}

// MD5 HMAC key calculation algorithm
func md5HMAC(password string, engineID string) []byte {
	comp := md5.New()
	var pi int // password index
	for i := 0; i < 1048576; i += 64 {
		var chunk []byte
		for e := 0; e < 64; e++ {
			chunk = append(chunk, password[pi%len(password)])
			pi++
		}
		comp.Write(chunk)
	}
	compressed := comp.Sum(nil)
	local := md5.New()
	local.Write(compressed)
	local.Write([]byte(engineID))
	local.Write(compressed)
	final := local.Sum(nil)
	return final
}

// SHA HMAC key calculation algorithm
func shaHMAC(password string, engineID string) []byte {
	hash := sha1.New()
	var pi int // password index
	for i := 0; i < 1048576; i += 64 {
		var chunk []byte
		for e := 0; e < 64; e++ {
			chunk = append(chunk, password[pi%len(password)])
			pi++
		}
		hash.Write(chunk)
	}
	hashed := hash.Sum(nil)
	local := sha1.New()
	local.Write(hashed)
	local.Write([]byte(engineID))
	local.Write(hashed)
	final := local.Sum(nil)
	return final
}

func genlocalkey(authProtocol SnmpV3AuthProtocol, passphrase string, engineID string) []byte {
	var secretKey []byte
	switch authProtocol {
	default:
		secretKey = md5HMAC(passphrase, engineID)
	case SHA:
		secretKey = shaHMAC(passphrase, engineID)
	}
	return secretKey
}

// http://tools.ietf.org/html/rfc2574#section-8.1.1.1
// localDESSalt needs to be incremented on every packet.
func (sp *UsmSecurityParameters) usmAllocateNewSalt() (interface{}, error) {
	var newSalt interface{}

	switch sp.PrivacyProtocol {
	case AES:
		newSalt = atomic.AddUint64(&(sp.localAESSalt), 1)
	default:
		newSalt = atomic.AddUint32(&(sp.localDESSalt), 1)
	}
	return newSalt, nil
}

func (sp *UsmSecurityParameters) usmSetSalt(newSalt interface{}) error {

	switch sp.PrivacyProtocol {
	case AES:
		aesSalt, ok := newSalt.(uint64)
		if !ok {
			return fmt.Errorf("salt provided to usmSetSalt is not the correct type for the AES privacy protocol")
		}
		var salt = make([]byte, 8)
		binary.BigEndian.PutUint64(salt, aesSalt)
		sp.PrivacyParameters = salt
	default:
		desSalt, ok := newSalt.(uint32)
		if !ok {
			return fmt.Errorf("salt provided to usmSetSalt is not the correct type for the DES privacy protocol")
		}
		var salt = make([]byte, 8)
		binary.BigEndian.PutUint32(salt, sp.AuthoritativeEngineBoots)
		binary.BigEndian.PutUint32(salt[4:], desSalt)
		sp.PrivacyParameters = salt
	}
	return nil
}

func (sp *UsmSecurityParameters) initPacket(packet *SnmpPacket) error {
	// http://tools.ietf.org/html/rfc2574#section-8.1.1.1
	// localDESSalt needs to be incremented on every packet.
	newSalt, err := sp.usmAllocateNewSalt()
	if err != nil {
		return err
	}
	if packet.MsgFlags&AuthPriv > AuthNoPriv {
		var s *UsmSecurityParameters
		if s, err = castUsmSecParams(packet.SecurityParameters); err != nil {
			return err
		}
		return s.usmSetSalt(newSalt)
	}

	return nil
}

func (sp *UsmSecurityParameters) encryptPacket(scopedPdu []byte) ([]byte, error) {
	var b []byte

	var privkey = genlocalkey(sp.AuthenticationProtocol,
		sp.PrivacyPassphrase,
		sp.AuthoritativeEngineID)

	switch sp.PrivacyProtocol {
	case AES:
		var iv [16]byte
		binary.BigEndian.PutUint32(iv[:], sp.AuthoritativeEngineBoots)
		binary.BigEndian.PutUint32(iv[4:], sp.AuthoritativeEngineTime)
		copy(iv[8:], sp.PrivacyParameters)

		block, err := aes.NewCipher(privkey[:16])
		if err != nil {
			return nil, err
		}
		stream := cipher.NewCFBEncrypter(block, iv[:])
		ciphertext := make([]byte, len(scopedPdu))
		stream.XORKeyStream(ciphertext, scopedPdu)
		pduLen, err := marshalLength(len(ciphertext))
		if err != nil {
			return nil, err
		}
		b = append([]byte{byte(OctetString)}, pduLen...)
		scopedPdu = append(b, ciphertext...)
	default:
		preiv := privkey[8:]
		var iv [8]byte
		for i := 0; i < len(iv); i++ {
			iv[i] = preiv[i] ^ sp.PrivacyParameters[i]
		}
		block, err := des.NewCipher(privkey[:8])
		if err != nil {
			return nil, err
		}
		mode := cipher.NewCBCEncrypter(block, iv[:])

		pad := make([]byte, des.BlockSize-len(scopedPdu)%des.BlockSize)
		scopedPdu = append(scopedPdu, pad...)

		ciphertext := make([]byte, len(scopedPdu))
		mode.CryptBlocks(ciphertext, scopedPdu)
		pduLen, err := marshalLength(len(ciphertext))
		if err != nil {
			return nil, err
		}
		b = append([]byte{byte(OctetString)}, pduLen...)
		scopedPdu = append(b, ciphertext...)
	}

	return scopedPdu, nil
}

func (sp *UsmSecurityParameters) decryptPacket(packet []byte, cursor int) ([]byte, error) {
	_, cursorTmp := parseLength(packet[cursor:])
	cursorTmp += cursor

	var privkey = genlocalkey(sp.AuthenticationProtocol,
		sp.PrivacyPassphrase,
		sp.AuthoritativeEngineID)

	switch sp.PrivacyProtocol {
	case AES:
		var iv [16]byte
		binary.BigEndian.PutUint32(iv[:], sp.AuthoritativeEngineBoots)
		binary.BigEndian.PutUint32(iv[4:], sp.AuthoritativeEngineTime)
		copy(iv[8:], sp.PrivacyParameters)

		block, err := aes.NewCipher(privkey[:16])
		if err != nil {
			return nil, err
		}
		stream := cipher.NewCFBDecrypter(block, iv[:])
		plaintext := make([]byte, len(packet[cursorTmp:]))
		stream.XORKeyStream(plaintext, packet[cursorTmp:])
		copy(packet[cursor:], plaintext)
		packet = packet[:cursor+len(plaintext)]
	default:
		if len(packet[cursorTmp:])%des.BlockSize != 0 {
			return nil, fmt.Errorf("Error decrypting ScopedPDU: not multiple of des block size.")
		}
		preiv := privkey[8:]
		var iv [8]byte
		for i := 0; i < len(iv); i++ {
			iv[i] = preiv[i] ^ sp.PrivacyParameters[i]
		}
		block, err := des.NewCipher(privkey[:8])
		if err != nil {
			return nil, err
		}
		mode := cipher.NewCBCDecrypter(block, iv[:])

		plaintext := make([]byte, len(packet[cursorTmp:]))
		mode.CryptBlocks(plaintext, packet[cursorTmp:])
		copy(packet[cursor:], plaintext)
		// truncate packet to remove extra space caused by the
		// octetstring/length header that was just replaced
		packet = packet[:cursor+len(plaintext)]
	}
	return packet, nil
}

// marshal a snmp version 3 security parameters field for the User Security Model
func (sp *UsmSecurityParameters) marshal(flags SnmpV3MsgFlags) ([]byte, uint32, error) {
	var buf bytes.Buffer
	var authParamStart uint32
	var err error

	// msgAuthoritativeEngineID
	buf.Write([]byte{byte(OctetString), byte(len(sp.AuthoritativeEngineID))})
	buf.WriteString(sp.AuthoritativeEngineID)

	// msgAuthoritativeEngineBoots
	msgAuthoritativeEngineBoots := marshalUvarInt(sp.AuthoritativeEngineBoots)
	buf.Write([]byte{byte(Integer), byte(len(msgAuthoritativeEngineBoots))})
	buf.Write(msgAuthoritativeEngineBoots)

	// msgAuthoritativeEngineTime
	msgAuthoritativeEngineTime := marshalUvarInt(sp.AuthoritativeEngineTime)
	buf.Write([]byte{byte(Integer), byte(len(msgAuthoritativeEngineTime))})
	buf.Write(msgAuthoritativeEngineTime)

	// msgUserName
	buf.Write([]byte{byte(OctetString), byte(len(sp.UserName))})
	buf.WriteString(sp.UserName)

	authParamStart = uint32(buf.Len() + 2) // +2 indicates PDUType + Length
	// msgAuthenticationParameters
	if flags&AuthNoPriv > 0 {
		buf.Write([]byte{byte(OctetString), 12,
			0, 0, 0, 0,
			0, 0, 0, 0,
			0, 0, 0, 0})
	} else {
		buf.Write([]byte{byte(OctetString), 0})
	}
	// msgPrivacyParameters
	if flags&AuthPriv > AuthNoPriv {
		privlen, err := marshalLength(len(sp.PrivacyParameters))
		if err != nil {
			return nil, 0, err
		}
		buf.Write([]byte{byte(OctetString)})
		buf.Write(privlen)
		buf.Write(sp.PrivacyParameters)
	} else {
		buf.Write([]byte{byte(OctetString), 0})
	}

	// wrap security parameters in a sequence
	paramLen, err := marshalLength(buf.Len())
	if err != nil {
		return nil, 0, err
	}
	tmpseq := append([]byte{byte(Sequence)}, paramLen...)
	authParamStart += uint32(len(tmpseq))
	tmpseq = append(tmpseq, buf.Bytes()...)

	return tmpseq, authParamStart, nil
}

func (sp *UsmSecurityParameters) unmarshal(flags SnmpV3MsgFlags, packet []byte, cursor int) (int, error) {

	var err error

	if PDUType(packet[cursor]) != Sequence {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model parameters\n")
	}
	_, cursorTmp := parseLength(packet[cursor:])
	cursor += cursorTmp

	rawMsgAuthoritativeEngineID, count, err := parseRawField(packet[cursor:], "msgAuthoritativeEngineID")
	if err != nil {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model msgAuthoritativeEngineID: %s", err.Error())
	}
	cursor += count
	if AuthoritativeEngineID, ok := rawMsgAuthoritativeEngineID.(string); ok {
		sp.AuthoritativeEngineID = AuthoritativeEngineID
		sp.Logger.Printf("Parsed authoritativeEngineID %s", AuthoritativeEngineID)
	}

	rawMsgAuthoritativeEngineBoots, count, err := parseRawField(packet[cursor:], "msgAuthoritativeEngineBoots")
	if err != nil {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model msgAuthoritativeEngineBoots: %s", err.Error())
	}
	cursor += count
	if AuthoritativeEngineBoots, ok := rawMsgAuthoritativeEngineBoots.(int); ok {
		sp.AuthoritativeEngineBoots = uint32(AuthoritativeEngineBoots)
		sp.Logger.Printf("Parsed authoritativeEngineBoots %d", AuthoritativeEngineBoots)
	}

	rawMsgAuthoritativeEngineTime, count, err := parseRawField(packet[cursor:], "msgAuthoritativeEngineTime")
	if err != nil {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model msgAuthoritativeEngineTime: %s", err.Error())
	}
	cursor += count
	if AuthoritativeEngineTime, ok := rawMsgAuthoritativeEngineTime.(int); ok {
		sp.AuthoritativeEngineTime = uint32(AuthoritativeEngineTime)
		sp.Logger.Printf("Parsed authoritativeEngineTime %d", AuthoritativeEngineTime)
	}

	rawMsgUserName, count, err := parseRawField(packet[cursor:], "msgUserName")
	if err != nil {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model msgUserName: %s", err.Error())
	}
	cursor += count
	if msgUserName, ok := rawMsgUserName.(string); ok {
		sp.UserName = msgUserName
		sp.Logger.Printf("Parsed userName %s", msgUserName)
	}

	rawMsgAuthParameters, count, err := parseRawField(packet[cursor:], "msgAuthenticationParameters")
	if err != nil {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model msgAuthenticationParameters: %s", err.Error())
	}
	if msgAuthenticationParameters, ok := rawMsgAuthParameters.(string); ok {
		sp.AuthenticationParameters = msgAuthenticationParameters
		sp.Logger.Printf("Parsed authenticationParameters %s", msgAuthenticationParameters)
	}
	// blank msgAuthenticationParameters to prepare for authentication check later
	if flags&AuthNoPriv > 0 {
		blank := make([]byte, 12)
		copy(packet[cursor+2:cursor+14], blank)
	}
	cursor += count

	rawMsgPrivacyParameters, count, err := parseRawField(packet[cursor:], "msgPrivacyParameters")
	if err != nil {
		return 0, fmt.Errorf("Error parsing SNMPV3 User Security Model msgPrivacyParameters: %s", err.Error())
	}
	cursor += count
	if msgPrivacyParameters, ok := rawMsgPrivacyParameters.(string); ok {
		sp.PrivacyParameters = []byte(msgPrivacyParameters)
		sp.Logger.Printf("Parsed privacyParameters %s", msgPrivacyParameters)
	}

	return cursor, nil
}