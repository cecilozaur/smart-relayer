package cluster

import (
	"bufio"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"container/heap"

	"github.com/gallir/smart-relayer/lib"
	"github.com/gallir/smart-relayer/redis"
	"github.com/mediocregopher/radix.v2/redis"
)

const (
	requestBufferSize = 8
)

type connHandler struct {
	sync.Mutex
	seq        uint64
	last       uint64
	pQueue     seqRespHeap // Priority queue for the responses
	srv        *Server
	conn       net.Conn
	buf        *bufio.ReadWriter
	reqCh      chan *reqData
	respCh     chan *redis.Resp
	senders    int32
	maxSenders int
}

func Handle(srv *Server, netCon net.Conn) {
	h := &connHandler{
		srv:  srv,
		conn: netCon,
		buf:  bufio.NewReadWriter(bufio.NewReader(netCon), bufio.NewWriter(netCon)),
	}
	defer h.close()

	h.init()

	reader := redis.NewRespReader(h.buf)
	for {
		err := netCon.SetReadDeadline(time.Now().Add(listenTimeout * time.Second))
		if err != nil {
			log.Printf("error setting read deadline: %s", err)
			return
		}

		req := reader.Read()
		if redis.IsTimeout(req) {
			continue
		} else if req.IsType(redis.IOErr) {
			return
		}

		resp := h.process(req)
		if h.srv.config.Compress || h.srv.config.Uncompress {
			resp = compress.UResp(resp)
		}
		resp.WriteTo(h.conn)
		h.buf.Flush()
	}
}

func (h *connHandler) init() {
	h.reqCh = make(chan *reqData, requestBufferSize)
	h.respCh = make(chan *redis.Resp, 1)
	if h.srv.config.Parallel {
		h.maxSenders = h.srv.config.MaxIdleConnections/2 + 1
		h.pQueue = make(seqRespHeap, 0, h.maxSenders)
		heap.Init(&h.pQueue)
	} else {
		h.maxSenders = 1
	}
	h.newSender()
}

func (h *connHandler) newSender() {
	if h.senders < int32(h.maxSenders) {
		go h.sender()
	}
}

func (h *connHandler) close() {
	if h.reqCh != nil {
		close(h.reqCh)
	}
	if h.respCh != nil {
		close(h.respCh)
	}
	h.buf.Flush()
	h.conn.Close()

}

func (h *connHandler) process(m *redis.Resp) *redis.Resp {
	ms, err := m.Array()
	if err != nil || len(ms) < 1 {
		return respBadCommand
	}

	cmd, err := ms[0].Str()
	if err != nil || strings.ToUpper(cmd) == selectCommand {
		return respBadCommand
	}

	h.seq++ //atomic.AddUint64(&h.seq, 1),
	data := &reqData{
		seq:      h.seq,
		cmd:      cmd,
		args:     ms[1:],
		compress: h.srv.config.Compress,
	}

	doAsync := false
	var fastResponse *redis.Resp
	if h.srv.mode == lib.ModeSmart {
		fastResponse, doAsync = commands[strings.ToUpper(cmd)]
	}

	if doAsync {
		h.reqCh <- data
		return fastResponse
	}

	data.answerCh = h.respCh
	h.reqCh <- data
	return <-h.respCh
}

func (h *connHandler) sender() {
	atomic.AddInt32(&h.senders, 1)
	defer atomic.AddInt32(&h.senders, -1)

	for m := range h.reqCh {
		// Add senders if there are pending requests
		if h.srv.config.Parallel && len(h.reqCh) > 0 {
			h.newSender()
		}
		args := make([]interface{}, len(m.args))
		for i, arg := range m.args {
			args[i] = arg
			if m.compress {
				b, e := arg.Bytes()
				if e == nil && len(b) > compress.MinCompressSize {
					args[i] = compress.Bytes(b)
				}
			}
		}

		if h.srv.config.Parallel {
			h.parallel(m, args)
			continue
		}

		m.resp = h.srv.pool.Cmd(m.cmd, args...)
		if m.answerCh != nil {
			m.answerCh <- m.resp
		}
	}
}

func (h *connHandler) parallel(m *reqData, args []interface{}) {
	h.Lock()
	heap.Push(&h.pQueue, m)
	h.Unlock()

	m.resp = h.srv.pool.Cmd(m.cmd, args...)

	h.Lock()
	for h.pQueue.Len() > 0 {
		item := heap.Pop(&h.pQueue).(*reqData)
		if item.resp == nil {
			heap.Push(&h.pQueue, item)
			break
		}
		if item.answerCh != nil {
			item.answerCh <- item.resp
		}
	}
	h.Unlock()
}