/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zjykzk/rocketmq-client-go/log"
	"github.com/zjykzk/rocketmq-client-go/message"
	"github.com/zjykzk/rocketmq-client-go/producer"
)

type statiBenchmarkProducerSnapshot struct {
	sendRequestSuccessCount     int64
	sendRequestFailedCount      int64
	receiveResponseSuccessCount int64
	receiveResponseFailedCount  int64
	sendMessageSuccessTimeTotal int64
	sendMessageMaxRT            int64
	createdAt                   time.Time
	next                        *statiBenchmarkProducerSnapshot
}

type produceSnapshots struct {
	sync.RWMutex
	head, tail, cur *statiBenchmarkProducerSnapshot
	len             int
}

func (s *produceSnapshots) takeSnapshot() {
	b := s.cur
	sn := new(statiBenchmarkProducerSnapshot)
	sn.sendRequestSuccessCount = atomic.LoadInt64(&b.sendRequestSuccessCount)
	sn.sendRequestFailedCount = atomic.LoadInt64(&b.sendRequestFailedCount)
	sn.receiveResponseSuccessCount = atomic.LoadInt64(&b.receiveResponseSuccessCount)
	sn.receiveResponseFailedCount = atomic.LoadInt64(&b.receiveResponseFailedCount)
	sn.sendMessageSuccessTimeTotal = atomic.LoadInt64(&b.sendMessageSuccessTimeTotal)
	sn.sendMessageMaxRT = atomic.LoadInt64(&b.sendMessageMaxRT)
	sn.createdAt = time.Now()

	s.Lock()
	if s.tail != nil {
		s.tail.next = sn
	}
	s.tail = sn
	if s.head == nil {
		s.head = s.tail
	}

	s.len++
	if s.len > 10 {
		s.head = s.head.next
		s.len--
	}
	s.Unlock()
}

func (s *produceSnapshots) printStati() {
	s.RLock()
	if s.len < 10 {
		s.RUnlock()
		return
	}

	f, l := s.head, s.tail
	respSucCount := float64(l.receiveResponseSuccessCount - f.receiveResponseSuccessCount)
	sendTps := respSucCount / l.createdAt.Sub(f.createdAt).Seconds()
	avgRT := float64(l.sendMessageSuccessTimeTotal-f.sendMessageSuccessTimeTotal) / respSucCount
	maxRT := atomic.LoadInt64(&s.cur.sendMessageMaxRT)
	s.RUnlock()

	fmt.Printf(
		"Send TPS: %d Max RT: %d Average RT: %7.3f Send Failed: %d Response Failed: %d Total:%d\n",
		int64(sendTps), maxRT, avgRT, l.sendRequestFailedCount, l.receiveResponseFailedCount, l.receiveResponseSuccessCount,
	)
}

type bproducer struct {
	topic         string
	nameSrv       string
	groupID       string
	instanceCount int
	testMinutes   int
	bodySize      int

	p *producer.Producer

	flags *flag.FlagSet
}

func init() {
	p := &bproducer{}
	flags := flag.NewFlagSet("producer", flag.ExitOnError)
	p.flags = flags

	flags.StringVar(&p.topic, "t", "BenchmarkTest", "topic name")
	flags.StringVar(&p.nameSrv, "n", "", "nameserver address")
	flags.StringVar(&p.groupID, "g", "benchmark_producer", "group id")
	flags.IntVar(&p.instanceCount, "i", 1, "instance count")
	flags.IntVar(&p.testMinutes, "m", 10, "test minutes")
	flags.IntVar(&p.bodySize, "s", 32, "body size")

	registerCommand("producer", p)
}

func (bp *bproducer) produceMsg(stati *statiBenchmarkProducerSnapshot, exit chan struct{}) {
AGAIN:
	select {
	case <-exit:
		return
	default:
	}

	now := time.Now()
	r, err := bp.p.SendSync(&message.Message{
		Topic: bp.topic, Body: buildMsg(bp.bodySize),
	})

	if err != nil {
		fmt.Printf("send message sync error:%s\n", err)
		goto AGAIN
	}

	if r.Status == producer.OK {
		atomic.AddInt64(&stati.receiveResponseSuccessCount, 1)
		atomic.AddInt64(&stati.sendRequestSuccessCount, 1)
		currentRT := int64(time.Since(now) / time.Millisecond)
		atomic.AddInt64(&stati.sendMessageSuccessTimeTotal, currentRT)
		prevRT := atomic.LoadInt64(&stati.sendMessageMaxRT)
		for currentRT > prevRT {
			if atomic.CompareAndSwapInt64(&stati.sendMessageMaxRT, prevRT, currentRT) {
				break
			}
			prevRT = atomic.LoadInt64(&stati.sendMessageMaxRT)
		}
	} else {
		//fmt.Printf("%s send message %s:%s status:%s\n", time.Now(), topic, tag, r.Status.String())
	}
	goto AGAIN
}

func (bp *bproducer) run(args []string) {
	var err error
	// setup trace
	f, err := os.Create("trace.out")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	err = trace.Start(f)
	if err != nil {
		panic(err)
	}
	defer trace.Stop()

	bp.flags.Parse(args)

	if bp.topic == "" {
		println("empty topic")
		bp.flags.Usage()
		return
	}

	if bp.groupID == "" {
		println("empty group id")
		bp.flags.Usage()
		return
	}

	if bp.nameSrv == "" {
		println("empty namesrv")
		bp.flags.Usage()
		return
	}
	if bp.instanceCount <= 0 {
		println("instance count must be positive integer")
		bp.flags.Usage()
		return
	}
	if bp.testMinutes <= 0 {
		println("test time must be positive integer")
		bp.flags.Usage()
		return
	}
	if bp.bodySize <= 0 {
		println("body size must be positive integer")
		bp.flags.Usage()
		return
	}

	stati := statiBenchmarkProducerSnapshot{}
	snapshots := produceSnapshots{cur: &stati}
	exitChan := make(chan struct{})
	wg := sync.WaitGroup{}

	p := producer.NewProducer(
		bp.groupID, strings.Split(bp.nameSrv, ","), &log.MockLogger{},
	)

	err = p.Start()
	if err != nil {
		fmt.Printf("start producer error:%s\n", err)
		return
	}
	defer p.Shutdown()
	bp.p = p

	for i := 0; i < bp.instanceCount; i++ {
		go func() {
			wg.Add(1)
			bp.produceMsg(&stati, exitChan)
			wg.Done()
		}()
	}

	// snapshot
	go func() {
		wg.Add(1)
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		for {
			select {
			case <-ticker.C:
				snapshots.takeSnapshot()
			case <-exitChan:
				ticker.Stop()
				return
			}
		}
	}()

	// print statistic
	go func() {
		wg.Add(1)
		defer wg.Done()
		ticker := time.NewTicker(time.Second * 10)
		for {
			select {
			case <-ticker.C:
				snapshots.printStati()
			case <-exitChan:
				ticker.Stop()
				return
			}
		}
	}()

	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-time.Tick(time.Minute * time.Duration(bp.testMinutes)):
	case <-signalChan:
	}

	close(exitChan)
	wg.Wait()
	snapshots.takeSnapshot()
	snapshots.printStati()
	fmt.Println("TEST DONE")
}

func (bp *bproducer) usage() {
	bp.flags.Usage()
}