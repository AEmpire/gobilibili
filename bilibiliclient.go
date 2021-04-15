package gobilibili

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strings"

	"time"

	"github.com/bitly/go-simplejson"
	"github.com/gorilla/websocket"
)

const (
	min = 1000000000
	max = 2000000000
)

//CmdType cmd const,Cmd开头的一系列常量
type CmdType string

const (
	//CmdAll 订阅所有cmd事件时使用
	CmdAll CmdType = ""
	//CmdLive 直播开始
	CmdLive CmdType = "LIVE"
	//CmdPreparing 直播准备中
	CmdPreparing CmdType = "PREPARING"
	//CmdDanmuMsg 弹幕消息
	CmdDanmuMsg CmdType = "DANMU_MSG"
	//CmdWelcomeGuard 管理进房
	CmdWelcomeGuard CmdType = "WELCOME_GUARD"
	//CmdWelcome 群众进房
	CmdWelcome CmdType = "WELCOME"
	//CmdSendGift 赠送礼物
	CmdSendGift CmdType = "SEND_GIFT"
	//CmdNoticeMsg 系统消息通知
	CmdNoticeMsg CmdType = "NOTICE_MSG"
	//CmdOnlineChange 在线人数变动,这不是一个标准cmd类型,仅为了统一handler接口而加入
	CmdOnlineChange CmdType = "ONLINE_CHANGE"
)

//Handler msg handler
type Handler interface {
	HandleFunc(c *Context) (breakHandleChain bool)
}

//HandleFunc convert func (Context) to Handler interface
type HandleFunc func(c *Context) bool

//HandleFunc this function calls HandleFunc itself to convert func to the interface
func (f HandleFunc) HandleFunc(context *Context) bool { return f(context) }

type roomInitResult struct {
	Code int `json:"code"`
	Data struct {
		Encrypted   bool `json:"encrypted"`
		HiddenTill  int  `json:"hidden_till"`
		IsHidden    bool `json:"is_hidden"`
		IsLocked    bool `json:"is_locked"`
		LockTill    int  `json:"lock_till"`
		NeedP2p     int  `json:"need_p2p"`
		PwdVerified bool `json:"pwd_verified"`
		RoomID      int  `json:"room_id"`
		ShortID     int  `json:"short_id"`
		UID         int  `json:"uid"`
	} `json:"data"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
}

type UserInfoResult struct {
	Code int `json:"code"`
	Data struct {
		Mid  int    `json:"mid"`
		Name string `json:"name"`
	} `json:"data"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
}

func GetRealRoomID(rid int) (realID int, mid int, err error) {
	resp, err := http.Get(fmt.Sprintf("http://api.live.bilibili.com/room/v1/Room/room_init?id=%d", rid))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var res roomInitResult
	jbytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if err = json.Unmarshal(jbytes, &res); err != nil {
		return
	}
	if res.Code == 0 {
		return res.Data.RoomID, res.Data.UID, nil
	}
	return 0, 0, fmt.Errorf(res.Message)
}

func GetRoomOwner(mid int) (name string, err error) {
	resp, err := http.Get(fmt.Sprintf("https://api.bilibili.com/x/space/acc/info?mid=%d", mid))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var res UserInfoResult
	jbytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if err = json.Unmarshal(jbytes, &res); err != nil {
		return
	}
	if res.Code == 0 {
		return res.Data.Name, nil
	}

	return "", fmt.Errorf(res.Message)
}

// BiliBiliClient define
type BiliBiliClient struct {
	roomID          int
	ChatPort        int
	protocolversion uint16
	ChatHost        string
	serverConn      *websocket.Conn
	uid             int
	handlerMap      map[CmdType]([]Handler)
	connected       bool
	Name            string
}

func NewBiliBiliClient() *BiliBiliClient {
	bili := new(BiliBiliClient)
	bili.ChatHost = "broadcastlv.chat.bilibili.com"
	bili.ChatPort = 443
	bili.protocolversion = 1
	bili.handlerMap = make(map[CmdType]([]Handler))

	return bili
}

//RegHandler Register a handler to the specified cmd
//cmd 	Cmd to bind,if empty，it will binds to all cmd
//handler
func (bili *BiliBiliClient) RegHandler(cmd CmdType, handler Handler) {
	funcAddr := append(bili.handlerMap[cmd], handler)
	bili.handlerMap[cmd] = funcAddr
}

//RegHandleFunc Register a handler function to the specified cmd
//cmd 	Cmd to bind,if empty，it will binds to all cmd
//handler
func (bili *BiliBiliClient) RegHandleFunc(cmd CmdType, hfunc HandleFunc) {
	bili.RegHandler(cmd, hfunc)
}

func (bili *BiliBiliClient) Disconnect() {
	bili.connected = false
	return
}

func (bili *BiliBiliClient) GetConnectStatus() bool {
	return bili.connected
}

// ConnectServer define
func (bili *BiliBiliClient) ConnectServer(roomID int) error {
	log.Println("Getting real room ID ....")
	roomID, mid, err := GetRealRoomID(roomID)
	if err != nil {
		return err
	}
	log.Println("Getting user name ....")
	uname, err := GetRoomOwner(mid)
	if err != nil {
		return err
	}
	log.Println("Entering room ....")
	dstAddr := fmt.Sprintf("wss://%s:%d/sub", bili.ChatHost, bili.ChatPort)
	dstConn, _, err := websocket.DefaultDialer.Dial(dstAddr, nil)
	if err != nil {
		return err
	}
	bili.serverConn = dstConn
	bili.roomID = roomID
	bili.Name = uname
	log.Println("弹幕链接中。。。")
	if err := bili.SendJoinChannel(roomID); err != nil {
		log.Println("err")
		return err
	}
	bili.connected = true
	go bili.heartbeatLoop()
	return bili.receiveMessageLoop()
}

// heartbeatLoop keep heartbeat and get online
func (bili *BiliBiliClient) heartbeatLoop() {
	for bili.connected {
		err := bili.sendSocketData(0, 16, bili.protocolversion, 2, 1, "")
		if err != nil {
			log.Printf("heartbeatError:%s\r\n", err.Error())
			return
		}
		time.Sleep(time.Second * 30)
	}
}

//GetRoomID Get the current room ID
func (bili *BiliBiliClient) GetRoomID() int { return bili.roomID }

// SendJoinChannel define
func (bili *BiliBiliClient) SendJoinChannel(channelID int) error {
	bili.uid = rand.Intn(max) + min
	body := fmt.Sprintf("{\"roomid\":%d,\"uid\":%d}", channelID, bili.uid)
	return bili.sendSocketData(0, 16, bili.protocolversion, 7, 1, body)
}

// sendSocketData define
func (bili *BiliBiliClient) sendSocketData(packetlength uint32, magic uint16, ver uint16, action uint32, param uint32, body string) error {
	bodyBytes := []byte(body)
	if packetlength == 0 {
		packetlength = uint32(len(bodyBytes) + 16)
	}
	headerBytes := new(bytes.Buffer)
	var data = []interface{}{
		packetlength,
		magic,
		ver,
		action,
		param,
	}
	for _, v := range data {
		err := binary.Write(headerBytes, binary.BigEndian, v)
		if err != nil {
			return err
		}
	}
	socketData := append(headerBytes.Bytes(), bodyBytes...)
	err := bili.serverConn.WriteMessage(websocket.BinaryMessage, socketData)
	return err
}

func (bili *BiliBiliClient) receiveMessageLoop() (err error) {
	defer CatchThrowHandle(func(e error) {
		bili.connected = false
		err = e
	})
	var oldOnline uint32
	for bili.connected {
		_, msg, err := bili.serverConn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) == 0 {
			continue
		}
		expr := binary.BigEndian.Uint32(msg[:4])
		ver := binary.BigEndian.Uint16(msg[6:8])
		num := binary.BigEndian.Uint32(msg[8:12])

		bLen := int(expr - 16)
		if bLen <= 0 {
			continue
		}
		num = num - 1
		switch num {
		case 0, 1, 2:
			num3 := binary.BigEndian.Uint32(msg[16:20])
			if oldOnline != num3 {
				oldOnline = num3
				sj := simplejson.New()
				sj.Set("cmd", CmdOnlineChange)
				sj.Set("online", num3)
				bili.callCmdHandlerChain(CmdOnlineChange, &Context{RoomID: bili.roomID, Msg: sj, Uname: bili.Name})
			}
		case 3, 4:
			buf := msg[16:expr]
			if ver == 2 {
				offset := 0

				b := bytes.NewReader(buf)
				r, err := zlib.NewReader(b)
				if err != nil {
					CatchAny(err)
				}

				buff := new(strings.Builder)
				_, err = io.Copy(buff, r)
				if err != nil {
					CatchAny(err)
				}

				messages := buff.String()

				buf = []byte(messages)
				for offset < len(buf) {
					packetLenExp := binary.BigEndian.Uint32(buf[offset : offset+4])
					packetLen := int(packetLenExp)
					data := buf[offset+16 : offset+packetLen]

					msg := string(data)
					i := strings.Index(msg, "{")
					if i >= 0 {
						CatchAny(bili.parseDanMu(msg[i:]))
					}
					offset += packetLen
				}
			} else {
				messages := string(buf)
				CatchAny(bili.parseDanMu(messages))
			}
		case 5, 6, 7:
		case 16:
		default:
		}
	}
	return err
}

func (bili *BiliBiliClient) parseDanMu(message string) (err error) {
	dic, err := simplejson.NewJson([]byte(message))
	if err != nil {
		return
	}
	cmd, err := dic.Get("cmd").String()
	if err != nil {
		err = nil
		return
	}
	// 弹幕升级了，弹幕cmd获得的值不是DANMU_MSG, 而是DANMU_MSG: + 版本, 例如: DANMU_MSG:4:0:2:2:2:0
	// 在这里兼容一下
	if strings.HasPrefix(cmd, string(CmdDanmuMsg)) {
		cmd = string(CmdDanmuMsg)
	}
	bili.callCmdHandlerChain(CmdType(cmd), &Context{RoomID: bili.roomID, Msg: dic, Uname: bili.Name}) //call cmd handler chain
	bili.callCmdHandlerChain("", &Context{RoomID: bili.roomID, Msg: dic, Uname: bili.Name})           //call default handler chain
	return nil
}
func (bili *BiliBiliClient) callCmdHandlerChain(cmd CmdType, c *Context) {
	fAddrs := bili.handlerMap[cmd] //call default handler
	for i := 0; i < len(fAddrs); i++ {
		if fAddrs[i].HandleFunc(c) {
			break
		}
	}
}
