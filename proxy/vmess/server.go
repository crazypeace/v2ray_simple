package vmess

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"

	"github.com/e1732a364fed/v2ray_simple/netLayer"
	"github.com/e1732a364fed/v2ray_simple/proxy"
	"github.com/e1732a364fed/v2ray_simple/utils"
	"go.uber.org/zap"
	"golang.org/x/crypto/chacha20poly1305"
)

func init() {
	proxy.RegisterServer(Name, &ServerCreator{})
}

type ServerCreator struct{}

func (ServerCreator) NewServerFromURL(url *url.URL) (proxy.Server, error) {
	return nil, utils.ErrNotImplemented
}

func (ServerCreator) NewServer(lc *proxy.ListenConf) (proxy.Server, error) {
	uuidStr := lc.Uuid

	s := NewServer()

	if uuidStr != "" {
		v2rayUser, err := utils.NewV2rayUser(uuidStr)
		if err != nil {
			return nil, err
		}
		s.addUser(v2rayUser)
	}

	if len(lc.Users) > 0 {
		us := utils.InitRealV2rayUsers(lc.Users)
		for _, u := range us {
			s.addUser(u)
		}
	}
	return s, nil

}

type Server struct {
	proxy.Base

	*utils.MultiUserMap

	authPairList []pair
}

type pair struct {
	utils.V2rayUser
	cipher.Block
}

func authUserByAuthPairList(bs []byte, authPairList []pair) (user utils.V2rayUser, ok bool) {
	for _, p := range authPairList {
		if tryMatchAuthIDByBlock(p.Block, bs) == 0 {
			return p.V2rayUser, true
		}
	}
	return
}

func NewServer() *Server {
	s := &Server{
		MultiUserMap: utils.NewMultiUserMap(),
	}
	s.SetUseUUIDStr_asKey()
	return s
}
func (s *Server) Name() string { return Name }

func (s *Server) addUser(u utils.V2rayUser) {
	s.MultiUserMap.AddUser_nolock(u)
	b, err := generateCipherByV2rayUser(u)
	if err != nil {
		panic(err)
	}
	p := pair{
		V2rayUser: u,
		Block:     b,
	}
	s.authPairList = append(s.authPairList, p)
}

func (s *Server) Handshake(underlay net.Conn) (result net.Conn, msgConn netLayer.MsgConn, targetAddr netLayer.Addr, returnErr error) {
	if err := proxy.SetCommonReadTimeout(underlay); err != nil {
		returnErr = err
		return
	}
	defer netLayer.PersistConn(underlay)

	data := utils.GetPacket()

	n, err := underlay.Read(data)
	if err != nil {
		returnErr = err
		return
	} else if n < utils.UUID_BytesLen {
		returnErr = utils.NumErr{E: utils.ErrInvalidData, N: 1}
		return
	}
	user, ok := authUserByAuthPairList(data[:utils.UUID_BytesLen], s.authPairList)
	if !ok {
		returnErr = utils.NumErr{E: utils.ErrInvalidData, N: 2}
		return
	}

	cmdKey := GetKey(user)
	remainBuf := bytes.NewBuffer(data[utils.UUID_BytesLen:n])

	aeadData, shouldDrain, bytesRead, errorReason := openAEADHeader(cmdKey, data[:16], remainBuf)
	if errorReason != nil {
		returnErr = errorReason

		if ce := utils.CanLogWarn("vmess openAEADHeader err"); ce != nil {
			ce.Write(zap.Any("things", []any{errorReason, shouldDrain, bytesRead}))
		}

		return
	}
	if len(aeadData) < 38 {
		returnErr = errors.New("len(aeadData)<38")
		return
	}

	//https://www.v2fly.org/developer/protocols/vmess.html#%E6%8C%87%E4%BB%A4%E9%83%A8%E5%88%86
	sc := &ServerConn{
		version:   int(aeadData[0]),
		Conn:      underlay,
		V2rayUser: user,
		reqRespV:  aeadData[33],
		opt:       aeadData[34],
		security:  aeadData[35] & 0x0f,
		cmd:       aeadData[37],
	}

	copy(sc.reqBodyIV[:], aeadData[1:17])
	copy(sc.reqBodyKey[:], aeadData[17:33])

	paddingLen := int(aeadData[35] >> 4)

	aeadDataBuf := bytes.NewBuffer(aeadData[38:])

	//todo: 防重放

	switch sc.cmd {
	//我们 不支持vmess 的 mux.cool
	case CmdTCP, CmdUDP:
		ad, err := GetAddrFrom(aeadDataBuf)
		if err != nil {
			returnErr = utils.NumErr{E: utils.ErrInvalidData, N: 3}
			return
		}
		sc.theTarget = ad
		targetAddr = ad
	}
	if paddingLen > 0 {
		tmpBs := aeadDataBuf.Next(paddingLen)
		if len(tmpBs) != paddingLen {
			returnErr = utils.NumErr{E: utils.ErrInvalidData, N: 4}
			return
		}
	}

	aeadDataBuf.Next(4)
	/*
		F := remainBuf.Next(4)
		fnv1a := fnv.New32a()
		fnv1a.Write(F)
	*/

	sc.remainBuf = remainBuf

	buf := utils.GetBuf()

	sc.aead_encodeRespHeader(buf)
	sc.Conn.Write(buf.Bytes())

	result = sc

	return
}

type ServerConn struct {
	net.Conn

	utils.V2rayUser
	version  int
	opt      byte
	security byte
	cmd      byte
	reqRespV byte

	theTarget netLayer.Addr

	reqBodyIV   [16]byte
	reqBodyKey  [16]byte
	respBodyIV  [16]byte
	respBodyKey [16]byte

	remainBuf *bytes.Buffer

	dataReader io.Reader
	dataWriter io.Writer
}

func (s *ServerConn) aead_encodeRespHeader(outBuf *bytes.Buffer) error {
	BodyKey := sha256.Sum256(s.reqBodyKey[:])
	copy(s.respBodyKey[:], BodyKey[:16])
	BodyIV := sha256.Sum256(s.reqBodyIV[:])
	copy(s.respBodyIV[:], BodyIV[:16])

	encryptionWriter := utils.GetBuf()
	encryptionWriter.Write([]byte{s.reqRespV, 0})
	encryptionWriter.Write([]byte{0x00, 0x00}) //我们暂时不支持动态端口，太复杂, 懒。

	aeadResponseHeaderLengthEncryptionKey := kdf16(s.respBodyKey[:], kdfSaltConstAEADRespHeaderLenKey)
	aeadResponseHeaderLengthEncryptionIV := kdf(s.respBodyIV[:], kdfSaltConstAEADRespHeaderLenIV)[:12]

	aeadResponseHeaderLengthEncryptionKeyAESBlock, _ := aes.NewCipher(aeadResponseHeaderLengthEncryptionKey)
	aeadResponseHeaderLengthEncryptionAEAD, _ := cipher.NewGCM(aeadResponseHeaderLengthEncryptionKeyAESBlock)

	aeadResponseHeaderLengthEncryptionBuffer := bytes.NewBuffer(nil)

	decryptedResponseHeaderLengthBinaryDeserializeBuffer := uint16(encryptionWriter.Len())

	binary.Write(aeadResponseHeaderLengthEncryptionBuffer, binary.BigEndian, decryptedResponseHeaderLengthBinaryDeserializeBuffer)

	AEADEncryptedLength := aeadResponseHeaderLengthEncryptionAEAD.Seal(nil, aeadResponseHeaderLengthEncryptionIV, aeadResponseHeaderLengthEncryptionBuffer.Bytes(), nil)
	io.Copy(outBuf, bytes.NewReader(AEADEncryptedLength))

	aeadResponseHeaderPayloadEncryptionKey := kdf16(s.respBodyKey[:], kdfSaltConstAEADRespHeaderPayloadKey)
	aeadResponseHeaderPayloadEncryptionIV := kdf(s.respBodyIV[:], kdfSaltConstAEADRespHeaderPayloadIV)[:12]

	aeadResponseHeaderPayloadEncryptionKeyAESBlock, _ := aes.NewCipher(aeadResponseHeaderPayloadEncryptionKey)
	aeadResponseHeaderPayloadEncryptionAEAD, _ := cipher.NewGCM(aeadResponseHeaderPayloadEncryptionKeyAESBlock)

	aeadEncryptedHeaderPayload := aeadResponseHeaderPayloadEncryptionAEAD.Seal(nil, aeadResponseHeaderPayloadEncryptionIV, encryptionWriter.Bytes(), nil)

	io.Copy(outBuf, bytes.NewReader(aeadEncryptedHeaderPayload))
	return nil

}

func (c *ServerConn) Write(b []byte) (n int, err error) {

	if c.dataWriter != nil {
		return c.dataWriter.Write(b)
	}

	c.dataWriter = c.Conn
	if c.opt&OptChunkStream == OptChunkStream {
		switch c.security {
		case SecurityNone:
			c.dataWriter = ChunkedWriter(c.Conn)

		case SecurityAES128GCM:
			block, _ := aes.NewCipher(c.respBodyKey[:])
			aead, _ := cipher.NewGCM(block)
			c.dataWriter = AEADWriter(c.Conn, aead, c.respBodyIV[:])

		case SecurityChacha20Poly1305:
			key := utils.GetBytes(32)
			t := md5.Sum(c.respBodyKey[:])
			copy(key, t[:])
			t = md5.Sum(key[:16])
			copy(key[16:], t[:])
			aead, _ := chacha20poly1305.New(key)
			c.dataWriter = AEADWriter(c.Conn, aead, c.respBodyIV[:])
			utils.PutBytes(key)
		}
	}

	return c.dataWriter.Write(b)
}

func (c *ServerConn) Read(b []byte) (n int, err error) {

	if c.dataReader != nil {
		return c.dataReader.Read(b)
	}
	var curReader io.Reader
	if c.remainBuf != nil && c.remainBuf.Len() > 0 {
		curReader = io.MultiReader(c.remainBuf, c.Conn)
	} else {
		curReader = c.Conn

	}

	if c.opt&OptChunkStream == OptChunkStream {
		switch c.security {
		case SecurityNone:
			c.dataReader = ChunkedReader(curReader)

		case SecurityAES128GCM:

			block, _ := aes.NewCipher(c.reqBodyKey[:])
			aead, _ := cipher.NewGCM(block)
			c.dataReader = AEADReader(curReader, aead, c.reqBodyIV[:])

		case SecurityChacha20Poly1305:
			key := utils.GetBytes(32)
			t := md5.Sum(c.reqBodyKey[:])
			copy(key, t[:])
			t = md5.Sum(key[:16])
			copy(key[16:], t[:])
			aead, _ := chacha20poly1305.New(key)
			c.dataReader = AEADReader(curReader, aead, c.reqBodyIV[:])
			utils.PutBytes(key)
		}
	}

	return c.dataReader.Read(b)

}
