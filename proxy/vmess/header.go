package vmess

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"time"

	"github.com/e1732a364fed/v2ray_simple/utils"
)

const (
	kdfSaltConstAuthIDEncryptionKey             = "AES Auth ID Encryption"
	kdfSaltConstAEADRespHeaderLenKey            = "AEAD Resp Header Len Key"
	kdfSaltConstAEADRespHeaderLenIV             = "AEAD Resp Header Len IV"
	kdfSaltConstAEADRespHeaderPayloadKey        = "AEAD Resp Header Key"
	kdfSaltConstAEADRespHeaderPayloadIV         = "AEAD Resp Header IV"
	kdfSaltConstVMessAEADKDF                    = "VMess AEAD KDF"
	kdfSaltConstVMessHeaderPayloadAEADKey       = "VMess Header AEAD Key"
	kdfSaltConstVMessHeaderPayloadAEADIV        = "VMess Header AEAD Nonce"
	kdfSaltConstVMessHeaderPayloadLengthAEADKey = "VMess Header AEAD Key_Length"
	kdfSaltConstVMessHeaderPayloadLengthAEADIV  = "VMess Header AEAD Nonce_Length"
)

func kdf(key []byte, path ...string) []byte {
	hmacCreator := &hMacCreator{value: []byte(kdfSaltConstVMessAEADKDF)}
	for _, v := range path {
		hmacCreator = &hMacCreator{value: []byte(v), parent: hmacCreator}
	}
	hmacf := hmacCreator.Create()
	hmacf.Write(key)
	return hmacf.Sum(nil)
}

func kdf16(key []byte, path ...string) []byte {
	r := kdf(key, path...)
	return r[:16]
}

type hMacCreator struct {
	parent *hMacCreator
	value  []byte
}

func (h *hMacCreator) Create() hash.Hash {
	if h.parent == nil {
		return hmac.New(sha256.New, h.value)
	}
	return hmac.New(h.parent.Create, h.value)
}

//https://github.com/v2fly/v2fly-github-io/issues/20
func createAuthID(cmdKey []byte, time int64) [16]byte {
	buf := &bytes.Buffer{}
	binary.Write(buf, binary.BigEndian, time)

	random := make([]byte, 4)
	rand.Read(random)
	buf.Write(random)
	zero := crc32.ChecksumIEEE(buf.Bytes())
	binary.Write(buf, binary.BigEndian, zero)

	aesBlock, _ := generateCipher(cmdKey)
	var result [16]byte
	aesBlock.Encrypt(result[:], buf.Bytes())
	return result
}

func generateCipher(cmdKey []byte) (cipher.Block, error) {
	return aes.NewCipher(kdf16(cmdKey, kdfSaltConstAuthIDEncryptionKey))
}
func generateCipherByV2rayUser(u utils.V2rayUser) (cipher.Block, error) {
	var fixedLengthCmdKey [16]byte
	copy(fixedLengthCmdKey[:], GetKey(u))
	return generateCipher(fixedLengthCmdKey[:])
}

//为0表示匹配成功
func tryMatchAuthIDByBlock(block cipher.Block, bs []byte) (failReason int) {

	var t int64
	var rand int32
	var zero uint32

	if len(bs) < utils.UUID_BytesLen {
		return 1
	}
	data := utils.GetBytes(utils.UUID_BytesLen)
	block.Decrypt(data, bs)

	buf := bytes.NewBuffer(data)

	binary.Read(buf, binary.BigEndian, &t)
	binary.Read(buf, binary.BigEndian, &rand)
	binary.Read(buf, binary.BigEndian, &zero)

	if zero != crc32.ChecksumIEEE(data[:12]) {
		return 2
	}

	if math.Abs(math.Abs(float64(t))-float64(time.Now().Unix())) > 120 {
		return 3
	}

	//todo: 用自己的代码 实现 防重放 机制
	return 0
}

func sealVMessAEADHeader(key [16]byte, data []byte, t time.Time) []byte {
	generatedAuthID := createAuthID(key[:], t.Unix())
	connectionNonce := make([]byte, 8)
	rand.Read(connectionNonce)

	aeadPayloadLengthSerializedByte := make([]byte, 2)
	binary.BigEndian.PutUint16(aeadPayloadLengthSerializedByte, uint16(len(data)))

	var payloadHeaderLengthAEADEncrypted []byte

	{
		payloadHeaderLengthAEADKey := kdf(key[:], kdfSaltConstVMessHeaderPayloadLengthAEADKey, string(generatedAuthID[:]), string(connectionNonce))[:16]
		payloadHeaderLengthAEADNonce := kdf(key[:], kdfSaltConstVMessHeaderPayloadLengthAEADIV, string(generatedAuthID[:]), string(connectionNonce))[:12]
		payloadHeaderLengthAEADAESBlock, _ := aes.NewCipher(payloadHeaderLengthAEADKey)
		payloadHeaderAEAD, _ := cipher.NewGCM(payloadHeaderLengthAEADAESBlock)
		payloadHeaderLengthAEADEncrypted = payloadHeaderAEAD.Seal(nil, payloadHeaderLengthAEADNonce, aeadPayloadLengthSerializedByte, generatedAuthID[:])
	}

	var payloadHeaderAEADEncrypted []byte

	{
		payloadHeaderAEADKey := kdf(key[:], kdfSaltConstVMessHeaderPayloadAEADKey, string(generatedAuthID[:]), string(connectionNonce))[:16]
		payloadHeaderAEADNonce := kdf(key[:], kdfSaltConstVMessHeaderPayloadAEADIV, string(generatedAuthID[:]), string(connectionNonce))[:12]
		payloadHeaderAEADAESBlock, _ := aes.NewCipher(payloadHeaderAEADKey)
		payloadHeaderAEAD, _ := cipher.NewGCM(payloadHeaderAEADAESBlock)
		payloadHeaderAEADEncrypted = payloadHeaderAEAD.Seal(nil, payloadHeaderAEADNonce, data, generatedAuthID[:])
	}

	outputBuffer := &bytes.Buffer{}

	outputBuffer.Write(generatedAuthID[:])
	outputBuffer.Write(payloadHeaderLengthAEADEncrypted)
	outputBuffer.Write(connectionNonce)
	outputBuffer.Write(payloadHeaderAEADEncrypted)

	return outputBuffer.Bytes()
}

//from v2fly/v2ray-core/proxy/vmess/aead/encrypt.go/OpenVMessAEADHeader.
// key 必须是16字节长. v2ray 的代码返回值没命名，不可取，我们加上。
func openAEADHeader(key []byte, authid []byte, remainDataReader io.Reader) (aeadData []byte, shouldDrain bool, bytesRead int, errorReason error) {

	var payloadHeaderLengthAEADEncrypted [18]byte
	var nonce [8]byte

	authidCheckValueReadBytesCounts, err := io.ReadFull(remainDataReader, payloadHeaderLengthAEADEncrypted[:])
	bytesRead += authidCheckValueReadBytesCounts
	if err != nil {
		return nil, false, bytesRead, err
	}
	nonceReadBytesCounts, err := io.ReadFull(remainDataReader, nonce[:])
	bytesRead += nonceReadBytesCounts
	if err != nil {

		return nil, false, bytesRead, err
	}

	var decryptedAEADHeaderLengthPayloadResult []byte

	{
		payloadHeaderLengthAEADKey := kdf16(key[:], kdfSaltConstVMessHeaderPayloadLengthAEADKey, string(authid[:]), string(nonce[:]))

		payloadHeaderLengthAEADNonce := kdf(key[:], kdfSaltConstVMessHeaderPayloadLengthAEADIV, string(authid[:]), string(nonce[:]))[:12]

		payloadHeaderAEADAESBlock, err := aes.NewCipher(payloadHeaderLengthAEADKey)
		if err != nil {
			panic(err.Error())
		}

		payloadHeaderLengthAEAD, err := cipher.NewGCM(payloadHeaderAEADAESBlock)
		if err != nil {
			panic(err.Error())
		}

		decryptedAEADHeaderLengthPayload, erropenAEAD := payloadHeaderLengthAEAD.Open(nil, payloadHeaderLengthAEADNonce, payloadHeaderLengthAEADEncrypted[:], authid[:])

		if erropenAEAD != nil {

			return nil, true, bytesRead, erropenAEAD
		}

		decryptedAEADHeaderLengthPayloadResult = decryptedAEADHeaderLengthPayload
	}

	var length uint16
	if err := binary.Read(bytes.NewReader(decryptedAEADHeaderLengthPayloadResult), binary.BigEndian, &length); err != nil {
		panic(err)
	}

	var decryptedAEADHeaderPayloadR []byte

	var payloadHeaderAEADEncryptedReadedBytesCounts int
	{
		payloadHeaderAEADKey := kdf16(key[:], kdfSaltConstVMessHeaderPayloadAEADKey, string(authid[:]), string(nonce[:]))

		payloadHeaderAEADNonce := kdf(key[:], kdfSaltConstVMessHeaderPayloadAEADIV, string(authid[:]), string(nonce[:]))[:12]

		// 16 == AEAD Tag size
		payloadHeaderAEADEncrypted := make([]byte, length+16)

		payloadHeaderAEADEncryptedReadedBytesCounts, err = io.ReadFull(remainDataReader, payloadHeaderAEADEncrypted)
		bytesRead += payloadHeaderAEADEncryptedReadedBytesCounts
		if err != nil {

			return nil, false, bytesRead, err
		}

		payloadHeaderAEADAESBlock, err := aes.NewCipher(payloadHeaderAEADKey)
		if err != nil {
			panic(err.Error())
		}

		payloadHeaderAEAD, err := cipher.NewGCM(payloadHeaderAEADAESBlock)
		if err != nil {
			panic(err.Error())
		}

		decryptedAEADHeaderPayload, erropenAEAD := payloadHeaderAEAD.Open(nil, payloadHeaderAEADNonce, payloadHeaderAEADEncrypted, authid[:])

		if erropenAEAD != nil {

			return nil, true, bytesRead, erropenAEAD
		}

		decryptedAEADHeaderPayloadR = decryptedAEADHeaderPayload
	}

	return decryptedAEADHeaderPayloadR, false, bytesRead, nil
}
