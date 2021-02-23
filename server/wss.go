package server

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"sync"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/muka/peer"
	"github.com/sirupsen/logrus"
)

// ClientMessage wrap a message received by a client
type ClientMessage struct {
	Client  IClient
	Message *peer.Message
}

//NewWebSocketServer create a new WebSocketServer
func NewWebSocketServer(realm IRealm, opts Options) *WebSocketServer {
	wss := WebSocketServer{
		Emitter:  peer.NewEmitter(),
		upgrader: websocket.Upgrader{},
		log:      createLogger("websocket-server", opts),
		realm:    realm,
		opts:     opts,
	}
	return &wss
}

// WebSocketServer wrap the websocket server
type WebSocketServer struct {
	peer.Emitter
	upgrader websocket.Upgrader
	clients  []*websocket.Conn
	cMutex   sync.Mutex
	log      *logrus.Entry
	realm    IRealm
	opts     Options
}

// Send send data to the clients
func (wss *WebSocketServer) Send(data []byte) {
	for _, conn := range wss.clients {
		err := conn.WriteMessage(websocket.BinaryMessage, data)
		if err != nil {
			wss.log.Warnf("Write failed: %s", err)
		}
	}
}

//onSocketConnection called when a client connect
func (wss *WebSocketServer) sendErrorAndClose(conn *websocket.Conn, msg string) error {
	err := conn.WriteJSON(peer.Message{
		Type: MessageTypeError,
		Payload: peer.Payload{
			Msg: msg,
		},
	})
	if err != nil {
		return err
	}
	err = conn.Close()
	if err != nil {
		return err
	}
	return nil
}

//
func (wss *WebSocketServer) configureWS(conn *websocket.Conn, client IClient) error {
	client.SetSocket(conn)
	go func() {
		for {

			messageType, raw, err := conn.ReadMessage()
			if err != nil {
				wss.log.Errorf("[%s] Read WS error: %s", client.GetID(), err)
				continue
			}

			// close
			if messageType == websocket.CloseMessage {
				if client.GetSocket() == conn {
					wss.realm.RemoveClientByID(client.GetID())
				}
				conn.Close()
				wss.Emit(WebsocketEventClose, client)
				return
			}

			// message handling
			data, err := ioutil.ReadAll(bytes.NewReader(raw))
			if err != nil {
				wss.log.Errorf("client message read error: %s", err)
				wss.Emit(WebsocketEventError, err)
				continue
			}

			message := new(peer.Message)
			err = json.Unmarshal(data, message)
			if err != nil {
				wss.log.Errorf("client message unmarshal error: %s", err)
				wss.Emit(WebsocketEventError, err)
				continue
			}

			message.Src = client.GetID()
			wss.Emit(WebsocketEventMessage, ClientMessage{client, message})
		}
	}()

	wss.Emit(WebsocketEventConnection, client)
	return nil
}

//registerClient
func (wss *WebSocketServer) registerClient(conn *websocket.Conn, id, token string) error {
	// Check concurrent limit
	clientsCount := len(wss.realm.GetClientsIds())

	if clientsCount >= wss.opts.ConcurrentLimit {
		err := wss.sendErrorAndClose(conn, ErrorConnectionLimitExceeded)
		if err != nil {
			wss.log.Errorf("[sendErrorAndClose] Error: %s", err)
		}
		return nil
	}

	client := NewClient(id, token)
	wss.realm.SetClient(client, id)

	err := conn.WriteJSON(peer.Message{Type: MessageTypeOpen})
	if err != nil {
		return err
	}

	err = wss.configureWS(conn, client)
	if err != nil {
		return err
	}
	return nil
}

//onSocketConnection called when a client connect
func (wss *WebSocketServer) onSocketConnection(conn *websocket.Conn, r *http.Request) {
	query := r.URL.Query()
	id := query.Get("id")
	token := query.Get("token")
	key := query.Get("key")

	if id == "" || token == "" || key == "" {
		err := wss.sendErrorAndClose(conn, ErrorInvalidWSParameters)
		if err != nil {
			wss.log.Errorf("[sendErrorAndClose] Error: %s", err)
		}
		return
	}

	if key != wss.opts.Key {
		err := wss.sendErrorAndClose(conn, ErrorInvalidKey)
		if err != nil {
			wss.log.Errorf("[sendErrorAndClose] Error: %s", err)
		}
		return
	}

	client := wss.realm.GetClientByID(id)

	if client == nil {
		err := wss.registerClient(conn, id, token)
		if err != nil {
			wss.log.Errorf("[registerClient] Error: %s", err)
		}
		return
	}

	if token != client.GetToken() {
		// ID-taken, invalid token
		err := conn.WriteJSON(peer.Message{
			Type: MessageTypeIDTaken,
			Payload: peer.Payload{
				Msg: "ID is taken",
			},
		})
		if err != nil {
			wss.log.Errorf("[%s] Failed to write message: %s", MessageTypeIDTaken, err)
		}
		conn.Close()
		return
	}

	wss.configureWS(conn, client)
	return
}

// Handler expose the http handler for websocket
func (wss *WebSocketServer) Handler() mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := wss.upgrader.Upgrade(w, r, nil)
			if err != nil {
				wss.log.Warnf("upgrade error: %s", err)
				next.ServeHTTP(w, r)
				return
			}

			wss.onSocketConnection(c, r)

			// defer c.Close()
			// for {
			// 	mt, message, err := c.ReadMessage()
			// 	if err != nil {
			// 		wss.log.Warnf("read: %s", err)
			// 		break
			// 	}
			// 	wss.log.Infof("recv: %s", message)
			// 	err = c.WriteMessage(mt, message)
			// 	if err != nil {
			// 		wss.log.Warnf("write: %s", err)
			// 		break
			// 	}
			// }
		})
	}
}
