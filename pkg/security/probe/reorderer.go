// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

// +build linux

package probe

import (
	"context"
	"time"

	"github.com/DataDog/ebpf/manager"
)

type reOrdererNodePool struct {
	head *reOrdererNode
}

func (p *reOrdererNodePool) alloc() *reOrdererNode {
	node := p.head
	if node != nil && node.timestamp == 0 {
		p.head = node.nextFree
		node.data = nil
		return node
	}

	return &reOrdererNode{}
}

func (p *reOrdererNodePool) free(node *reOrdererNode) {
	node.timestamp = 0

	if p.head == nil {
		p.head = node
	} else {
		node.nextFree = p.head
		p.head = node
	}
}

type reOrdererList struct {
	head *reOrdererNode
	tail *reOrdererNode
	size uint64
}

type reOrdererNode struct {
	timestamp uint64
	data      []byte
	next      *reOrdererNode
	prev      *reOrdererNode
	nextFree  *reOrdererNode
}

func (l *reOrdererList) append(node *reOrdererNode) {
	l.size++

	if l.head == nil {
		l.head = node
		l.tail = node

		return
	}

	var prev *reOrdererNode

	curr := l.tail
	for curr != nil {
		if node.timestamp >= curr.timestamp {
			if prev != nil {
				prev.prev = node
			} else {
				l.tail = node
			}
			node.next = prev
			curr.next = node
			node.prev = curr

			return
		}

		prev = curr
		curr = curr.prev
	}

	l.head.prev = node
	node.next = l.head
	l.head = node
}

// ReOrdererOpts options to pass when creating a new instance of ReOrderer
type ReOrdererOpts struct {
	QueueSize  uint64        // size of the chan where the perf data are pushed
	WindowSize uint64        // number of element to keep for orderering
	Delay      time.Duration // delay to wait before handling an element outside of the window in millisecond
	Rate       time.Duration // delay between two time based iterations
}

// ReOrderer defines an event re-orderer
type ReOrderer struct {
	queue           chan []byte
	handler         func(data []byte)
	list            *reOrdererList
	pool            *reOrdererNodePool
	timestampGetter func(data []byte) (uint64, error)
	opts            ReOrdererOpts
}

// Start event handler loop
func (r *ReOrderer) Start(ctx context.Context) {
	ticker := time.NewTicker(r.opts.Rate)
	defer ticker.Stop()

	dequeue := func(predicate func(node *reOrdererNode) bool) {
		curr := r.list.head
		for curr != nil && predicate(curr) {
			if len(curr.data) != 0 {
				r.handler(curr.data)
			}
			next := curr.next

			r.pool.free(curr)

			curr = next
			r.list.size--
		}

		r.list.head = curr
		if curr == nil {
			r.list.tail = nil
		} else {
			curr.prev = nil
		}
	}

	var lastTm, tm uint64
	var err error

	for {
		select {
		case data := <-r.queue:
			if len(data) > 0 {
				tm, err = r.timestampGetter(data)
				if err != nil {
					continue
				}
			} else {
				tm = lastTm
			}

			if tm == 0 {
				continue
			}
			lastTm = tm

			node := r.pool.alloc()
			node.timestamp = tm
			node.data = data

			r.list.append(node)

			dequeue(func(node *reOrdererNode) bool {
				if r.list.size < r.opts.WindowSize {
					return false
				}
				return true
			})
		case <-ticker.C:
			tail := r.list.tail
			if tail == nil {
				continue
			}

			size := r.list.size + uint64(len(r.queue))
			if size > r.opts.WindowSize {
				continue
			}

			// fill up in 3 pass
			for i := uint64(0); i <= r.opts.WindowSize/3; i++ {
				r.queue <- nil
			}
		case <-ctx.Done():
			return
		}
	}
}

// HandleEvent handle event form perf ring
func (r *ReOrderer) HandleEvent(CPU int, data []byte, perfMap *manager.PerfMap, manager *manager.Manager) {
	r.queue <- data
}

// NewReOrderer returns a new ReOrderer
func NewReOrderer(handler func([]byte), tsg func(data []byte) (uint64, error), opts ReOrdererOpts) *ReOrderer {
	return &ReOrderer{
		queue:           make(chan []byte, opts.QueueSize),
		handler:         handler,
		list:            &reOrdererList{},
		pool:            &reOrdererNodePool{},
		timestampGetter: tsg,
		opts:            opts,
	}
}
