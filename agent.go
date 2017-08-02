// Copyright (c) nano Author. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package nano

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/lonnng/nano/codec"
	"github.com/lonnng/nano/message"
	"github.com/lonnng/nano/packet"
	"github.com/lonnng/nano/session"
	"log"
)

const agentWriteBacklog = 16

var (
	ErrBrokenPipe   = errors.New("broken low-level pipe")
	ErrBufferExceed = errors.New("session send buffer exceed")
)

// Agent corresponding a user, used for store raw socket information
type (
	agent struct {
		lastAt  int64               // last heartbeat unix time stamp
		socket  net.Conn            // low-level socket fd
		chSend  chan pendingMessage // push message queue
		state   int32               // current agent state
		chDie   chan struct{}       // wait for close
		codec   *codec.Decoder      // coder & decoder
		session *session.Session    // session
	}

	pendingMessage struct {
		typ     message.MessageType // message type
		route   string              // message route(push)
		mid     uint                // response message id(response)
		payload interface{}         // payload
	}
)

// Create new agent instance
func newAgent(conn net.Conn) *agent {
	a := &agent{
		socket: conn,
		lastAt: time.Now().Unix(),
		chSend: make(chan pendingMessage, agentWriteBacklog),
		state:  statusStart,
		chDie:  make(chan struct{}),
		codec:  codec.NewDecoder(),
	}

	// binding session
	s := session.New(a)
	a.session = s

	return a
}

func (a *agent) status() int32 {
	return atomic.LoadInt32(&a.state)
}

func (a *agent) setStatus(state int32) {
	atomic.StoreInt32(&a.state, state)
}

func (a *agent) write() {
	ticker := time.NewTicker(env.heartbeat)
	chWrite := make(chan []byte, agentWriteBacklog)
	// clean func
	defer func() {
		ticker.Stop()
		close(a.chSend)
		close(chWrite)
	}()

	for {
		select {
		case <-ticker.C:
			deadline := time.Now().Add(-2 * env.heartbeat).Unix()
			if a.lastAt < deadline {
				log.Println(fmt.Sprintf("Session heartbeat timeout, LastTime=%d, Deadline=%d", a.lastAt, deadline))
				a.socket.Close()
				return
			}
			chWrite <- heartbeatPacket

		case data := <-chWrite:
			// close agent while low-level socket broken
			if _, err := a.socket.Write(data); err != nil {
				log.Println(err.Error())
				a.socket.Close()
				return
			}

		case data := <-a.chSend:
			payload, err := serializeOrRaw(data.payload)
			if err != nil {
				log.Println(err.Error())
				break
			}

			// construct message and encode
			m := &message.Message{
				Type:  data.typ,
				Data:  payload,
				Route: data.route,
				ID:    data.mid,
			}
			em, err := m.Encode()
			if err != nil {
				log.Println(err.Error())
				break
			}

			// packet encode
			p, err := codec.Encode(packet.Data, em)
			if err != nil {
				log.Println(err)
				break
			}
			chWrite <- p

		case <-a.chDie: // agent closed signal
			return
		}
	}
}

// String, implementation for Stringer interface
func (a *agent) String() string {
	return fmt.Sprintf("Remote=%s, LastTime=%d", a.socket.RemoteAddr().String(), a.lastAt)
}

func (a *agent) heartbeat() {
	a.lastAt = time.Now().Unix()
}

func (a *agent) Close() {
	if a.state == statusClosed {
		return
	}

	a.state = statusClosed
	log.Println(fmt.Sprintf("Session closed, Id=%d, IP=%s", a.session.ID, a.socket.RemoteAddr()))

	// close all channel
	close(a.chDie)
	a.socket.Close()
}

func (a *agent) Push(route string, v interface{}) error {
	if a.status() == statusClosed {
		return ErrBrokenPipe
	}

	if len(a.chSend) >= agentWriteBacklog {
		return ErrBufferExceed
	}

	a.chSend <- pendingMessage{typ: message.Push, route: route, payload: v}
	return nil
}

// Response message to session
func (a *agent) Response(v interface{}) error {
	mid := a.session.LastRID
	if mid <= 0 {
		return ErrSessionOnNotify
	}

	if len(a.chSend) >= agentWriteBacklog {
		return ErrBufferExceed
	}

	a.chSend <- pendingMessage{typ: message.Response, mid: mid, payload: v}
	return nil
}