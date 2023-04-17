// Copyright 2022 The AmazeChain Authors
// This file is part of the AmazeChain library.
//
// The AmazeChain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The AmazeChain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the AmazeChain library. If not, see <http://www.gnu.org/licenses/>.

package event

import (
	"errors"
	"reflect"
	"sync"
)

var errBadChannel = errors.New("event: Subscribe argument does not have sendable channel type")

const firstSubSendCase = 1

type Subscription interface {
	Err() <-chan error // returns the error channel
	Unsubscribe()      // cancels sending of events, closing the error channel
}

type Event struct {
	once      sync.Once
	sendLock  chan struct{}
	removeSub chan interface{}
	sendCases caseList

	mu    sync.Mutex
	inbox caseList
	etype reflect.Type
}

type eventTypeError struct {
	got, want reflect.Type
	op        string
}

func (e eventTypeError) Error() string {
	return "event: wrong type in " + e.op + " got " + e.got.String() + ", want " + e.want.String()
}

func (e *Event) init() {
	e.removeSub = make(chan interface{})
	e.sendLock = make(chan struct{}, 1)
	e.sendLock <- struct{}{}
	e.sendCases = caseList{{Chan: reflect.ValueOf(e.removeSub), Dir: reflect.SelectRecv}}
}

func (e *Event) Subscribe(channel interface{}) Subscription {
	e.once.Do(e.init)

	chanval := reflect.ValueOf(channel)
	chantyp := chanval.Type()
	if chantyp.Kind() != reflect.Chan || chantyp.ChanDir()&reflect.SendDir == 0 {
		panic(errBadChannel)
	}
	sub := &eventSub{feed: e, channel: chanval, err: make(chan error, 1)}

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.typeCheck(chantyp.Elem()) {
		panic(eventTypeError{op: "Subscribe", got: chantyp, want: reflect.ChanOf(reflect.SendDir, e.etype)})
	}

	cas := reflect.SelectCase{Dir: reflect.SelectSend, Chan: chanval}
	e.inbox = append(e.inbox, cas)
	return sub
}

func (e *Event) typeCheck(typ reflect.Type) bool {
	if e.etype == nil {
		e.etype = typ
		return true
	}
	return e.etype == typ
}

func (e *Event) remove(sub *eventSub) {

	ch := sub.channel.Interface()
	e.mu.Lock()
	index := e.inbox.find(ch)
	if index != -1 {
		e.inbox = e.inbox.delete(index)
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	select {
	case e.removeSub <- ch:
	case <-e.sendLock:
		e.sendCases = e.sendCases.delete(e.sendCases.find(ch))
		e.sendLock <- struct{}{}
	}
}

func (e *Event) Send(value interface{}) (nsent int) {
	rvalue := reflect.ValueOf(value)

	e.once.Do(e.init)
	<-e.sendLock

	e.mu.Lock()
	e.sendCases = append(e.sendCases, e.inbox...)
	e.inbox = nil

	if !e.typeCheck(rvalue.Type()) {
		e.sendLock <- struct{}{}
		e.mu.Unlock()
		panic(eventTypeError{op: "Send", got: rvalue.Type(), want: e.etype})
	}
	e.mu.Unlock()

	for i := firstSubSendCase; i < len(e.sendCases); i++ {
		e.sendCases[i].Send = rvalue
	}

	cases := e.sendCases
	for {
		for i := firstSubSendCase; i < len(cases); i++ {
			if cases[i].Chan.TrySend(rvalue) {
				nsent++
				cases = cases.deactivate(i)
				i--
			}
		}
		if len(cases) == firstSubSendCase {
			break
		}
		chosen, recv, _ := reflect.Select(cases)
		if chosen == 0 /* <-f.removeSub */ {
			index := e.sendCases.find(recv.Interface())
			e.sendCases = e.sendCases.delete(index)
			if index >= 0 && index < len(cases) {
				cases = e.sendCases[:len(cases)-1]
			}
		} else {
			cases = cases.deactivate(chosen)
			nsent++
		}
	}

	for i := firstSubSendCase; i < len(e.sendCases); i++ {
		e.sendCases[i].Send = reflect.Value{}
	}
	e.sendLock <- struct{}{}
	return nsent
}

type eventSub struct {
	feed    *Event
	channel reflect.Value
	errOnce sync.Once
	err     chan error
}

func (sub *eventSub) Unsubscribe() {
	sub.errOnce.Do(func() {
		sub.feed.remove(sub)
		close(sub.err)
	})
}

func (sub *eventSub) Err() <-chan error {
	return sub.err
}

type caseList []reflect.SelectCase

func (cs caseList) find(channel interface{}) int {
	for i, cas := range cs {
		if cas.Chan.Interface() == channel {
			return i
		}
	}
	return -1
}

func (cs caseList) delete(index int) caseList {
	return append(cs[:index], cs[index+1:]...)
}

func (cs caseList) deactivate(index int) caseList {
	last := len(cs) - 1
	cs[index], cs[last] = cs[last], cs[index]
	return cs[:last]
}