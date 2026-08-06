package dealer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

const (
	pingInterval = 30 * time.Second
	timeout      = 10 * time.Second

	// reconnectMaxElapsedTime bounds how long recvLoop keeps retrying a dropped
	// connection before giving up. The default backoff budget is 15 minutes;
	// during that whole window every remote-control command (including a seek)
	// would silently go nowhere. Giving up promptly instead hands the player
	// back to the daemon, which rebuilds the session from scratch.
	reconnectMaxElapsedTime = 30 * time.Second
)

var ErrDealerClosed = errors.New("dealer closed")

type Dealer struct {
	log librespot.Logger

	client *http.Client

	addr        librespot.GetAddressFunc
	accessToken librespot.GetLogin5TokenFunc

	conn *websocket.Conn

	done         chan struct{}
	closeOnce    sync.Once
	recvLoopOnce sync.Once
	lastPong     time.Time
	lastPongLock sync.Mutex

	// dispatchCh hands parsed "message"/"request" frames from recvLoop to
	// dispatchLoop. Handling a request blocks until the consumer replies (up to
	// the daemon's 30s command timeout, e.g. while loading a new track), so it
	// must not run on recvLoop's own goroutine: that would stall reading the
	// next frame off the wire, including the pong replies the ping/pong
	// watchdog depends on. Buffered so a short burst of frames doesn't stall
	// recvLoop either; a single dispatcher goroutine preserves arrival order.
	dispatchCh chan *RawMessage

	// connMu protects conn pointer state.
	connMu sync.RWMutex

	messageReceivers     []messageReceiver
	messageReceiversLock sync.RWMutex

	requestReceivers     map[string]requestReceiver
	requestReceiversLock sync.RWMutex
}

func NewDealer(log librespot.Logger, client *http.Client, dealerAddr librespot.GetAddressFunc, accessToken librespot.GetLogin5TokenFunc) *Dealer {
	return &Dealer{
		client: &http.Client{
			Transport:     client.Transport,
			CheckRedirect: client.CheckRedirect,
			Jar:           client.Jar,
			Timeout:       timeout,
		},
		log:              log,
		addr:             dealerAddr,
		accessToken:      accessToken,
		done:             make(chan struct{}),
		dispatchCh:       make(chan *RawMessage, 8),
		requestReceivers: map[string]requestReceiver{},
	}
}

func (d *Dealer) Connect(ctx context.Context) error {
	select {
	case <-d.done:
		return ErrDealerClosed
	default:
	}

	d.connMu.RLock()
	alreadyConnected := d.conn != nil
	d.connMu.RUnlock()
	if alreadyConnected {
		d.log.Debugf("dealer connection already opened")
		return nil
	}

	return d.connect(ctx)
}

// connect dials a fresh dealer connection and installs it as the current one.
// connMu is only held for the brief pointer swap, not for the dial itself:
// callers may retry this in a loop for a while on a bad network, and holding
// a write lock for that whole time would block every reader of connMu
// (writeConn/closeConn, used by the ping/pong watchdog and by anything
// sending a reply) for just as long, leaving a stuck reconnect undetectable
// until it finally gives up.
func (d *Dealer) connect(ctx context.Context) error {
	accessToken, err := d.accessToken(ctx, false)
	if err != nil {
		return fmt.Errorf("failed obtaining dealer access token: %w", err)
	}

	addr := d.addr(ctx)
	conn, _, err := websocket.Dial(ctx, fmt.Sprintf("wss://%s/?access_token=%s", addr, accessToken), &websocket.DialOptions{
		HTTPClient: d.client,
		HTTPHeader: http.Header{
			"User-Agent": []string{librespot.UserAgent()},
		},
	})
	if err != nil {
		return err
	}

	// remove the read limit before publishing the connection
	conn.SetReadLimit(math.MaxUint32)

	d.connMu.Lock()
	oldConn := d.conn
	d.conn = conn
	d.connMu.Unlock()

	if oldConn != nil {
		_ = oldConn.Close(websocket.StatusServiceRestart, "")
	}

	d.log.Debug(fmt.Sprintf("connected to %s", addr))
	return nil
}

func (d *Dealer) Close() {
	d.closeOnce.Do(func() {
		close(d.done)
		d.closeConn(websocket.StatusGoingAway)
	})
}

func (d *Dealer) startReceiving() {
	d.recvLoopOnce.Do(func() {
		d.log.Tracef("starting dealer recv loop")
		d.resetPongDeadline()
		go d.pingTicker()
		go d.dispatchLoop()
		go d.recvLoop()
	})
}

func (d *Dealer) pingTicker() {
	ticker := time.NewTicker(pingInterval)

loop:
	for {
		select {
		case <-d.done:
			break loop
		case <-ticker.C:
			timePassed := d.timeSinceLastPong()
			if timePassed > pingInterval+timeout {
				d.log.Errorf("did not receive last pong from dealer, %.0fs passed", timePassed.Seconds())

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				d.closeConn(websocket.StatusServiceRestart)
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			conn, err := d.writeConn(ctx, websocket.MessageText, []byte("{\"type\":\"ping\"}"))
			cancel()
			d.log.Tracef("sent dealer ping")

			if err != nil {
				select {
				case <-d.done:
					break loop
				default:
				}

				d.log.WithError(err).Warnf("failed sending dealer ping")

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				d.closeConnRef(conn, websocket.StatusServiceRestart)
				continue
			}
		}
	}

	ticker.Stop()
}

func (d *Dealer) recvLoop() {
loop:
	for {
		select {
		case <-d.done:
			break loop
		default:
			// no need to hold the connMu since reconnection happens in this routine
			msgType, messageBytes, err := d.readConn(context.Background())

			// don't log closed error if we're shutting down
			if err != nil {
				select {
				case <-d.done:
					if websocket.CloseStatus(err) == websocket.StatusGoingAway {
						d.log.Debugf("dealer connection closed")
					}
					break loop
				default:
				}

				d.log.WithError(err).Errorf("failed receiving dealer message")
				break loop
			} else if msgType != websocket.MessageText {
				d.log.WithError(err).Warnf("unsupported message type: %v, len: %d", msgType, len(messageBytes))
				continue
			}

			var message RawMessage
			if err := json.Unmarshal(messageBytes, &message); err != nil {
				d.log.WithError(err).Error("failed unmarshalling dealer message")
				break loop
			}

			switch message.Type {
			case "message", "request":
				// Handed off rather than handled here: processing a request
				// blocks until the daemon replies, which must not stall this
				// loop's next read (see dispatchCh's doc comment).
				select {
				case d.dispatchCh <- &message:
				case <-d.done:
					break loop
				}
			case "ping":
				// we never receive ping messages
				break
			case "pong":
				d.lastPongLock.Lock()
				d.lastPong = time.Now()
				d.lastPongLock.Unlock()
				d.log.Tracef("received dealer pong")
				break
			default:
				d.log.Warnf("unknown dealer message type: %s", message.Type)
				break
			}
		}
	}

	// always close as we might end up here because of application errors
	d.closeConn(websocket.StatusInternalError)

	select {
	case <-d.done:
	default:
		b := backoff.NewExponentialBackOff()
		b.MaxElapsedTime = reconnectMaxElapsedTime
		if err := backoff.Retry(d.reconnect, b); err != nil {
			d.log.WithError(err).Errorf("failed reconnecting dealer")

			// something went very wrong, give up
			d.Close()
		} else {
			// reconnection was successful, do not close receivers
			return
		}
	}

	d.requestReceiversLock.RLock()
	for _, recv := range d.requestReceivers {
		close(recv.c)
	}
	d.requestReceiversLock.RUnlock()

	d.messageReceiversLock.RLock()
	for _, recv := range d.messageReceivers {
		close(recv.c)
	}
	d.messageReceiversLock.RUnlock()

	d.log.Debugf("dealer recv loop stopped")
}

func (d *Dealer) sendReply(key string, success bool) error {
	reply := Reply{Type: "reply", Key: key}
	reply.Payload.Success = success

	replyBytes, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("failed marshalling reply: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	_, err = d.writeConn(ctx, websocket.MessageText, replyBytes)
	cancel()
	if err != nil {
		return fmt.Errorf("failed sending dealer reply: %w", err)
	}

	return nil
}

func (d *Dealer) reconnect() error {
	if err := d.connect(context.TODO()); err != nil {
		return err
	}

	d.resetPongDeadline()
	// restart the recv loop
	go d.recvLoop()

	d.log.Debugf("re-established dealer connection")
	return nil
}

func (d *Dealer) resetPongDeadline() {
	d.lastPongLock.Lock()
	d.lastPong = time.Now().Add(pingInterval)
	d.lastPongLock.Unlock()
}

func (d *Dealer) timeSinceLastPong() time.Duration {
	d.lastPongLock.Lock()
	defer d.lastPongLock.Unlock()
	return time.Since(d.lastPong)
}

func (d *Dealer) closeConn(status websocket.StatusCode) {
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()

	d.closeConnRef(conn, status)
}

func (d *Dealer) closeConnRef(conn *websocket.Conn, status websocket.StatusCode) {
	if conn != nil {
		_ = conn.Close(status, "")
	}
}

func (d *Dealer) writeConn(ctx context.Context, typ websocket.MessageType, payload []byte) (*websocket.Conn, error) {
	d.connMu.RLock()
	select {
	case <-d.done:
		d.connMu.RUnlock()
		return nil, ErrDealerClosed
	default:
	}

	conn := d.conn
	d.connMu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("dealer connection not established")
	}

	err := conn.Write(ctx, typ, payload)
	if err != nil {
		select {
		case <-d.done:
			return conn, ErrDealerClosed
		default:
		}
	}

	return conn, err
}

func (d *Dealer) readConn(ctx context.Context) (websocket.MessageType, []byte, error) {
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()

	if conn == nil {
		return 0, nil, fmt.Errorf("dealer connection not established")
	}

	return conn.Read(ctx)
}
