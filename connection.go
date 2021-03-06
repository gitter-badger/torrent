package torrent

import (
	"bufio"
	"container/list"
	"encoding"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/anacrolix/libtorgo/bencode"

	"github.com/anacrolix/torrent/internal/pieceordering"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

var optimizedCancels = expvar.NewInt("optimizedCancels")

type peerSource byte

const (
	peerSourceIncoming = 'I'
	peerSourceDHT      = 'H'
	peerSourcePEX      = 'X'
)

// Maintains the state of a connection with a peer.
type connection struct {
	conn      net.Conn
	rw        io.ReadWriter // The real slim shady
	encrypted bool
	Discovery peerSource
	uTP       bool
	closing   chan struct{}
	mu        sync.Mutex // Only for closing.
	post      chan pp.Message
	writeCh   chan []byte

	// The connections preferred order to download pieces.
	piecePriorities []int
	// The piece request order based on piece priorities.
	pieceRequestOrder *pieceordering.Instance

	UnwantedChunksReceived int
	UsefulChunksReceived   int

	lastMessageReceived     time.Time
	completedHandshake      time.Time
	lastUsefulChunkReceived time.Time

	// Stuff controlled by the local peer.
	Interested       bool
	Choked           bool
	Requests         map[request]struct{}
	requestsLowWater int
	// Indexed by metadata piece, set to true if posted and pending a
	// response.
	metadataRequests []bool

	// Stuff controlled by the remote peer.
	PeerID             [20]byte
	PeerInterested     bool
	PeerChoked         bool
	PeerRequests       map[request]struct{}
	PeerExtensionBytes peerExtensionBytes
	// Whether the peer has the given piece. nil if they've not sent any
	// related messages yet.
	PeerPieces []bool
	peerHasAll bool

	PeerMaxRequests  int // Maximum pending requests the peer allows.
	PeerExtensionIDs map[string]byte
	PeerClientName   string
}

func newConnection() (c *connection) {
	c = &connection{
		Choked:          true,
		PeerChoked:      true,
		PeerMaxRequests: 250,

		closing: make(chan struct{}),
		writeCh: make(chan []byte),
		post:    make(chan pp.Message),
	}
	return
}

func (cn *connection) remoteAddr() net.Addr {
	return cn.conn.RemoteAddr()
}

func (cn *connection) localAddr() net.Addr {
	return cn.conn.LocalAddr()
}

// Adjust piece position in the request order for this connection based on the
// given piece priority.
func (cn *connection) pendPiece(piece int, priority piecePriority) {
	if priority == piecePriorityNone {
		cn.pieceRequestOrder.DeletePiece(piece)
		return
	}
	pp := cn.piecePriorities[piece]
	// Priority goes to Now, then Next in connection order. Then Readahead in
	// by piece index. Then normal again by connection order.
	key := func() int {
		switch priority {
		case piecePriorityNow:
			return -3*len(cn.piecePriorities) + 3*pp
		case piecePriorityNext:
			return -2*len(cn.piecePriorities) + 2*pp
		case piecePriorityReadahead:
			return -len(cn.piecePriorities) + pp
		case piecePriorityNormal:
			return pp
		default:
			panic(priority)
		}
	}()
	cn.pieceRequestOrder.SetPiece(piece, key)
}

func (cn *connection) supportsExtension(ext string) bool {
	_, ok := cn.PeerExtensionIDs[ext]
	return ok
}

func (cn *connection) completedString(t *torrent) string {
	if cn.PeerPieces == nil && !cn.peerHasAll {
		return "?"
	}
	return fmt.Sprintf("%d/%d", func() int {
		if cn.peerHasAll {
			if t.haveInfo() {
				return t.numPieces()
			}
			return -1
		}
		ret := 0
		for _, b := range cn.PeerPieces {
			if b {
				ret++
			}
		}
		return ret
	}(), func() int {
		if cn.peerHasAll || cn.PeerPieces == nil {
			if t.haveInfo() {
				return t.numPieces()
			}
			return -1
		}
		return len(cn.PeerPieces)
	}())
}

// Correct the PeerPieces slice length. Return false if the existing slice is
// invalid, such as by receiving badly sized BITFIELD, or invalid HAVE
// messages.
func (cn *connection) setNumPieces(num int) error {
	if cn.peerHasAll {
		return nil
	}
	if cn.PeerPieces == nil {
		return nil
	}
	if len(cn.PeerPieces) == num {
	} else if len(cn.PeerPieces) < num {
		cn.PeerPieces = append(cn.PeerPieces, make([]bool, num-len(cn.PeerPieces))...)
	} else if len(cn.PeerPieces) <= (num+7)/8*8 {
		for _, have := range cn.PeerPieces[num:] {
			if have {
				return errors.New("peer has invalid piece")
			}
		}
		cn.PeerPieces = cn.PeerPieces[:num]
	} else {
		return fmt.Errorf("peer bitfield is excessively long: expected %d, have %d", num, len(cn.PeerPieces))
	}
	if len(cn.PeerPieces) != num {
		panic("wat")
	}
	return nil
}

func eventAgeString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%.2fs ago", time.Now().Sub(t).Seconds())
}

// Inspired by https://trac.transmissionbt.com/wiki/PeerStatusText
func (cn *connection) statusFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.Interested {
		c('i')
	}
	if cn.Choked {
		c('c')
	}
	c('-')
	if cn.encrypted {
		c('E')
	}
	if cn.Discovery != 0 {
		c(byte(cn.Discovery))
	}
	if cn.uTP {
		c('T')
	}
	c('-')
	if cn.PeerInterested {
		c('i')
	}
	if cn.PeerChoked {
		c('c')
	}
	return
}

func (cn *connection) WriteStatus(w io.Writer, t *torrent) {
	// \t isn't preserved in <pre> blocks?
	fmt.Fprintf(w, "%q: %s-%s\n", cn.PeerID, cn.localAddr(), cn.remoteAddr())
	fmt.Fprintf(w, "    last msg: %s, connected: %s, last useful chunk: %s\n",
		eventAgeString(cn.lastMessageReceived),
		eventAgeString(cn.completedHandshake),
		eventAgeString(cn.lastUsefulChunkReceived))
	fmt.Fprintf(w, "    %s completed, good chunks: %d/%d reqs: %d-%d, flags: %s\n",
		cn.completedString(t),
		cn.UsefulChunksReceived,
		cn.UnwantedChunksReceived+cn.UsefulChunksReceived,
		len(cn.Requests),
		len(cn.PeerRequests),
		cn.statusFlags())
}

func (c *connection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closing:
		return
	default:
	}
	close(c.closing)
	// TODO: This call blocks sometimes, why?
	go c.conn.Close()
}

func (c *connection) PeerHasPiece(piece int) bool {
	if c.peerHasAll {
		return true
	}
	if piece >= len(c.PeerPieces) {
		return false
	}
	return c.PeerPieces[piece]
}

func (c *connection) Post(msg pp.Message) {
	select {
	case c.post <- msg:
	case <-c.closing:
	}
}

func (c *connection) RequestPending(r request) bool {
	_, ok := c.Requests[r]
	return ok
}

func (c *connection) requestMetadataPiece(index int) {
	eID := c.PeerExtensionIDs["ut_metadata"]
	if eID == 0 {
		return
	}
	if index < len(c.metadataRequests) && c.metadataRequests[index] {
		return
	}
	c.Post(pp.Message{
		Type:       pp.Extended,
		ExtendedID: eID,
		ExtendedPayload: func() []byte {
			b, err := bencode.Marshal(map[string]int{
				"msg_type": pp.RequestMetadataExtensionMsgType,
				"piece":    index,
			})
			if err != nil {
				panic(err)
			}
			return b
		}(),
	})
	for index >= len(c.metadataRequests) {
		c.metadataRequests = append(c.metadataRequests, false)
	}
	c.metadataRequests[index] = true
}

func (c *connection) requestedMetadataPiece(index int) bool {
	return index < len(c.metadataRequests) && c.metadataRequests[index]
}

// Returns true if more requests can be sent.
func (c *connection) Request(chunk request) bool {
	if len(c.Requests) >= c.PeerMaxRequests {
		return false
	}
	if !c.PeerHasPiece(int(chunk.Index)) {
		return true
	}
	if c.RequestPending(chunk) {
		return true
	}
	c.SetInterested(true)
	if c.PeerChoked {
		return false
	}
	if c.Requests == nil {
		c.Requests = make(map[request]struct{}, c.PeerMaxRequests)
	}
	c.Requests[chunk] = struct{}{}
	c.requestsLowWater = len(c.Requests) / 2
	c.Post(pp.Message{
		Type:   pp.Request,
		Index:  chunk.Index,
		Begin:  chunk.Begin,
		Length: chunk.Length,
	})
	return true
}

// Returns true if an unsatisfied request was canceled.
func (c *connection) Cancel(r request) bool {
	if c.Requests == nil {
		return false
	}
	if _, ok := c.Requests[r]; !ok {
		return false
	}
	delete(c.Requests, r)
	c.Post(pp.Message{
		Type:   pp.Cancel,
		Index:  r.Index,
		Begin:  r.Begin,
		Length: r.Length,
	})
	return true
}

// Returns true if an unsatisfied request was canceled.
func (c *connection) PeerCancel(r request) bool {
	if c.PeerRequests == nil {
		return false
	}
	if _, ok := c.PeerRequests[r]; !ok {
		return false
	}
	delete(c.PeerRequests, r)
	return true
}

func (c *connection) Choke() {
	if c.Choked {
		return
	}
	c.Post(pp.Message{
		Type: pp.Choke,
	})
	c.Choked = true
}

func (c *connection) Unchoke() {
	if !c.Choked {
		return
	}
	c.Post(pp.Message{
		Type: pp.Unchoke,
	})
	c.Choked = false
}

func (c *connection) SetInterested(interested bool) {
	if c.Interested == interested {
		return
	}
	c.Post(pp.Message{
		Type: func() pp.MessageType {
			if interested {
				return pp.Interested
			} else {
				return pp.NotInterested
			}
		}(),
	})
	c.Interested = interested
}

// Writes buffers to the socket from the write channel.
func (conn *connection) writer() {
	// Reduce write syscalls.
	buf := bufio.NewWriterSize(conn.rw, 0x8000) // 32 KiB
	// Receives when buf is not empty.
	notEmpty := make(chan struct{}, 1)
	for {
		if buf.Buffered() != 0 {
			// Make sure it's receivable.
			select {
			case notEmpty <- struct{}{}:
			default:
			}
		}
		select {
		case b, ok := <-conn.writeCh:
			if !ok {
				return
			}
			_, err := buf.Write(b)
			if err != nil {
				conn.Close()
				return
			}
		case <-conn.closing:
			return
		case <-notEmpty:
			err := buf.Flush()
			if err != nil {
				return
			}
		}
	}
}

func (conn *connection) writeOptimizer(keepAliveDelay time.Duration) {
	defer close(conn.writeCh) // Responsible for notifying downstream routines.
	pending := list.New()     // Message queue.
	var nextWrite []byte      // Set to nil if we need to need to marshal the next message.
	timer := time.NewTimer(keepAliveDelay)
	defer timer.Stop()
	lastWrite := time.Now()
	for {
		write := conn.writeCh // Set to nil if there's nothing to write.
		if pending.Len() == 0 {
			write = nil
		} else if nextWrite == nil {
			var err error
			nextWrite, err = pending.Front().Value.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				panic(err)
			}
		}
	event:
		select {
		case <-timer.C:
			if pending.Len() != 0 {
				break
			}
			keepAliveTime := lastWrite.Add(keepAliveDelay)
			if time.Now().Before(keepAliveTime) {
				timer.Reset(keepAliveTime.Sub(time.Now()))
				break
			}
			pending.PushBack(pp.Message{Keepalive: true})
		case msg, ok := <-conn.post:
			if !ok {
				return
			}
			if msg.Type == pp.Cancel {
				for e := pending.Back(); e != nil; e = e.Prev() {
					elemMsg := e.Value.(pp.Message)
					if elemMsg.Type == pp.Request && msg.Index == elemMsg.Index && msg.Begin == elemMsg.Begin && msg.Length == elemMsg.Length {
						pending.Remove(e)
						optimizedCancels.Add(1)
						break event
					}
				}
			}
			pending.PushBack(msg)
		case write <- nextWrite:
			pending.Remove(pending.Front())
			nextWrite = nil
			lastWrite = time.Now()
			if pending.Len() == 0 {
				timer.Reset(keepAliveDelay)
			}
		case <-conn.closing:
			return
		}
	}
}
