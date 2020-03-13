/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watch

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FullChannelBehavior controls how the Broadcaster reacts if a watcher's watch
// channel is full.
type FullChannelBehavior int

const (
	WaitIfChannelFull FullChannelBehavior = iota  //iota，特殊常量，可以被编译器修改的常量; iota 可理解为 const 语句块中的行索引
	DropIfChannelFull
)

// Buffer the incoming queue a little bit even though it should rarely ever accumulate
// anything, just in case a few events are received in such a short window that
// Broadcaster can't move them onto the watchers' queues fast enough.
const incomingQueueLength = 25

// Broadcaster distributes（分发） event notifications among any number of watchers. Every event
// is delivered to every watcher.  每个 event 发送到了 每个 watcher
type Broadcaster struct {
	// TODO: see if this lock is needed now that new watchers go through
	// the incoming channel.
	lock sync.Mutex

	watchers     map[int64]*broadcasterWatcher
	nextWatcher  int64
	// WaitGroup 计数器，
	// 它有三个方法：Add(), Done(), Wait() 用来控制计数器的数量。
	// Add(n) 把计数器设置为n ，Done() 每次把计数器-1 ，wait() 会阻塞代码的运行，直到计数器地值减为0。
	distributing sync.WaitGroup

	incoming chan Event

	// How large to make watcher's channel.
	watchQueueLength int
	// If one of the watch channels is full, don't wait for it to become empty.
	// Instead just deliver it to the watchers that do have space in their
	// channels and move on to the next event.
	// It's more fair to do this on a per-watcher basis than to do it on the
	// "incoming" channel, which would allow one slow watcher to prevent all
	// other watchers from getting new events.
	fullChannelBehavior FullChannelBehavior
}

// NewBroadcaster creates a new Broadcaster. queueLength is the maximum number of events to queue per watcher.
// It is guaranteed that events will be distributed in the order in which they occur,
// but the order in which a single event is distributed among all of the watchers is unspecified.
func NewBroadcaster(queueLength int, fullChannelBehavior FullChannelBehavior) *Broadcaster {
	m := &Broadcaster{
		watchers:            map[int64]*broadcasterWatcher{},
		incoming:            make(chan Event, incomingQueueLength), // channel Event 的缓存大小
		watchQueueLength:    queueLength,  // 每个watcher的queue的大小
		fullChannelBehavior: fullChannelBehavior,  // 当channel 满了，是 WaitIfChannelFull or  DropIfChannelFull
	}
	// 置为计数器为1
	m.distributing.Add(1)
	// receives from m.incoming and distributes to all watchers.
	go m.loop()
	return m
}

const internalRunFunctionMarker = "internal-do-function"

// a function type we can shoehorn into the queue.
type functionFakeRuntimeObject func()

func (obj functionFakeRuntimeObject) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}
func (obj functionFakeRuntimeObject) DeepCopyObject() runtime.Object {
	if obj == nil {
		return nil
	}
	// funcs are immutable. Hence, just return the original func.
	return obj
}

// Execute f, blocking the incoming queue (and waiting for it to drain first).
// The purpose of this terrible hack is so that watchers added after an event
// won't ever see that event, and will always see any event after they are
// added.
func (b *Broadcaster) blockQueue(f func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	b.incoming <- Event{
		Type: internalRunFunctionMarker,
		Object: functionFakeRuntimeObject(func() {
			defer wg.Done()
			f()
		}),
	}
	// 阻塞
	wg.Wait()
}

// Watch adds a new watcher to the list and returns an Interface for it.
// Note: new watchers will only receive new events. They won't get an entire history
// of previous events.
// 新的watcher 只会接收新的 events，不会处理全部历史的 events
func (m *Broadcaster) Watch() Interface {

	var w *broadcasterWatcher
	m.blockQueue(func() {
		m.lock.Lock()
		defer m.lock.Unlock()
		id := m.nextWatcher
		m.nextWatcher++
		w = &broadcasterWatcher{
			result:  make(chan Event, m.watchQueueLength),
			stopped: make(chan struct{}),
			id:      id,
			m:       m,
		}
		m.watchers[id] = w
	})
	return w
}

// WatchWithPrefix adds a new watcher to the list and returns an Interface for it. It sends
// queuedEvents down the new watch before beginning to send ordinary events from Broadcaster.
// The returned watch will have a queue length that is at least large enough to accommodate
// all of the items in queuedEvents.
func (m *Broadcaster) WatchWithPrefix(queuedEvents []Event) Interface {
	var w *broadcasterWatcher
	m.blockQueue(func() {
		m.lock.Lock()
		defer m.lock.Unlock()
		id := m.nextWatcher
		m.nextWatcher++
		length := m.watchQueueLength
		if n := len(queuedEvents) + 1; n > length {
			length = n
		}
		w = &broadcasterWatcher{
			result:  make(chan Event, length),
			stopped: make(chan struct{}),
			id:      id,
			m:       m,
		}
		m.watchers[id] = w
		for _, e := range queuedEvents {
			w.result <- e
		}
	})
	return w
}

// stopWatching stops the given watcher and removes it from the list.
func (m *Broadcaster) stopWatching(id int64) {
	m.lock.Lock()
	defer m.lock.Unlock()
	w, ok := m.watchers[id]
	if !ok {
		// No need to do anything, it's already been removed from the list.
		return
	}
	delete(m.watchers, id)
	close(w.result)
}

// closeAll disconnects all watchers (presumably in response to a Shutdown call).
func (m *Broadcaster) closeAll() {
	m.lock.Lock()
	defer m.lock.Unlock()
	for _, w := range m.watchers {
		close(w.result)
	}
	// Delete everything from the map, since presence/absence in the map is used
	// by stopWatching to avoid double-closing the channel.
	m.watchers = map[int64]*broadcasterWatcher{}
}

// Action distributes the given event among all watchers.
func (m *Broadcaster) Action(action EventType, obj runtime.Object) {
	m.incoming <- Event{action, obj}
}

// Shutdown disconnects all watchers (but any queued events will still be distributed).
// You must not call Action or Watch* after calling Shutdown. This call blocks
// until all events have been distributed through the outbound channels. Note
// that since they can be buffered, this means that the watchers might not
// have received the data yet as it can remain sitting in the buffered
// channel.
func (m *Broadcaster) Shutdown() {
	close(m.incoming)
	m.distributing.Wait()
}

// loop receives from m.incoming and distributes to all watchers.
// 循环接收来自 m.incoming 的 event 然后分发给所有的 watchers
func (m *Broadcaster) loop() {
	// Deliberately not catching crashes here. Yes, bring down the process if there's a
	// bug in watch.Broadcaster.
	for event := range m.incoming {
		if event.Type == internalRunFunctionMarker {
			event.Object.(functionFakeRuntimeObject)()
			continue
		}
		// distribute sends event to all watchers. Blocking.
		// 将事件分发发送给所有观察者
		m.distribute(event)
	}
	m.closeAll()
	// 消耗一个计数器值， 即计数器-1
	m.distributing.Done()
}

// distribute sends event to all watchers. Blocking.
// 分发将事件发送给所有观察者
func (m *Broadcaster) distribute(event Event) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.fullChannelBehavior == DropIfChannelFull {  // 如果channel full 则丢弃，就是 有 default 语句 非阻塞
		// 在通常情况下，select 语句会阻塞当前 Goroutine 并等待多个 Channel 中的一个达到可以收发的状态。
		// 但是如果 select 控制结构中包含 default 语句，那么这个 select 语句在执行时会遇到以下两种情况：
		// 1. 当存在可以收发的 Channel 时，直接处理该 Channel 对应的 case；
		// 2. 当不存在可以收发的 Channel 是，执行 default 中的语句
		for _, w := range m.watchers {
			select {  // select 关键字 等待 case表达式返回；返回则继续向下执行（执行case里的代码—— 此处case没有语句）
			case w.result <- event:  // 如果 w.result 接收 event， 则向下执行（执行case里的代码—— 此处case没有语句）
			case <-w.stopped:  // 如果 w.stopped 表达式返回，则向下执行；
			default: // Don't block if the event can't be queued.
			}
		}
	} else {
		for _, w := range m.watchers {
			select {
			case w.result <- event:
			case <-w.stopped:
			}
		}
	}
}

// broadcasterWatcher handles a single watcher of a broadcaster
// broadcasterWatcher 处理一个 broadcaster的一个 watcher
type broadcasterWatcher struct {
	result  chan Event
	stopped chan struct{}
	stop    sync.Once
	id      int64
	m       *Broadcaster
}

// ResultChan returns a channel to use for waiting on events.
func (mw *broadcasterWatcher) ResultChan() <-chan Event {
	return mw.result
}

// Stop stops watching and removes mw from its list.
func (mw *broadcasterWatcher) Stop() {
	mw.stop.Do(func() {
		close(mw.stopped)
		mw.m.stopWatching(mw.id)
	})
}
