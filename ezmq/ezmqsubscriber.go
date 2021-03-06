/*******************************************************************************
 * Copyright 2017 Samsung Electronics All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 *******************************************************************************/

package ezmq

import (
	proto "github.com/golang/protobuf/proto"
	zmq "github.com/pebbe/zmq4"
	"go.uber.org/zap"

	List "container/list"
	"math/rand"
	"strconv"
	"time"
	"strings"
)

// Address prefix to bind subscriber.
const SUB_TCP_PREFIX = "tcp://"
const INPROC_PREFIX = "inproc://shutdown-"

// Callback to get all the subscribed events.
type EZMQSubCB func(event EZMQMessage)

// Callback to get all the subscribed events for a specific topic.
type EZMQSubTopicCB func(topic string, event EZMQMessage)

// Structure represents EZMQSubscriber.
type EZMQSubscriber struct {
	ip               string
	port             int
	subCallback      EZMQSubCB
	subTopicCallback EZMQSubTopicCB

	context        *zmq.Context
	subscriber     *zmq.Socket
	shutdownServer *zmq.Socket
	shutdownClient *zmq.Socket
	poller         *zmq.Poller
	shutdownChan   chan string

	isReceiverStarted bool
}

// Contructs EZMQSubscriber.
func GetEZMQSubscriber(ip string, port int, subCallback EZMQSubCB, subTopicCallback EZMQSubTopicCB) *EZMQSubscriber {
	var instance *EZMQSubscriber
	instance = &EZMQSubscriber{}
	instance.ip = ip
	instance.port = port
	instance.subCallback = subCallback
	instance.subTopicCallback = subTopicCallback
	instance.context = GetInstance().getContext()
	InitLogger()
	if nil == instance.context {
		logger.Error("Context is null")
		return nil
	}
	instance.subscriber = nil
	instance.shutdownServer = nil
	instance.shutdownClient = nil
	instance.poller = nil
	instance.shutdownChan = nil
	instance.isReceiverStarted = false
	return instance
}

func parseSocketData(subInstance *EZMQSubscriber) {
	var frame1 []byte
	var frame2 []byte
	var frame3 []byte
	var event Event
	var ezmqByteData EZMQByteData
	var isTopic bool = false
	var topic string
	var more bool = false
	var err error

	if nil == subInstance.subscriber {
		logger.Error("subscriber is null")
		return
	}

	frame1, err = subInstance.subscriber.RecvBytes(0)
	if err == nil {
		more, _ = subInstance.subscriber.GetRcvmore()
		if true == more {
			frame2, err = subInstance.subscriber.RecvBytes(0)
			if err == nil {
				more, _ = subInstance.subscriber.GetRcvmore()
				if true == more {
					frame3, err = subInstance.subscriber.RecvBytes(0)
					isTopic = true
				}
			}
		}
	}
	if false == isTopic {
		frame3 = frame2[:]
		frame2 = frame1[:]
	} else {
		topic = string(frame1[:])
		if strings.HasSuffix(topic, "/") {
			topic = topic[:len(topic)-len("/")]
		} 
	}

	//Parse header
	ezmqHeader := frame2[0]
	var contentType byte = (ezmqHeader >> 5)

	// Parse the data
	if EZMQ_CONTENT_TYPE_PROTOBUF == contentType {
		//change byte array to Event
		err = proto.Unmarshal(frame3, &event)
		if nil != err {
			logger.Error("Error in unmarshalling data")
		}
		if isTopic {
			subInstance.subTopicCallback(topic, event)
		} else {
			subInstance.subCallback(event)
		}
	} else if EZMQ_CONTENT_TYPE_BYTEDATA == contentType {
		ezmqByteData.ByteData = frame3
		if isTopic {
			subInstance.subTopicCallback(topic, ezmqByteData)
		} else {
			subInstance.subCallback(ezmqByteData)
		}
	} else {
		logger.Error("Not a supported type")
	}
}

func receive(subInstance *EZMQSubscriber) {
	var sockets []zmq.Polled
	var socket zmq.Polled
	var soc *zmq.Socket
	var err error

	for {
		sockets, err = subInstance.poller.Poll(-1)
		if err == nil {
			for _, socket = range sockets {
				switch soc = socket.Socket; soc {
				case subInstance.subscriber:
					parseSocketData(subInstance)
				case subInstance.shutdownClient:
					logger.Debug("Received shut down request")
					goto End
				}
			}
		}
	}
End:
	if nil != subInstance.shutdownChan {
		logger.Debug("Go routine stopped: signaling channel")
		subInstance.shutdownChan <- "shutdown"
	}
}

// Starts SUB instance.
func (subInstance *EZMQSubscriber) Start() EZMQErrorCode {
	if nil == subInstance.context {
		logger.Error("Context is null")
		return EZMQ_ERROR
	}

	var err error
	var address = getInProcUniqueAddress()
	if nil == subInstance.shutdownServer {
		subInstance.shutdownServer, err = zmq.NewSocket(zmq.PAIR)
		if nil != err {
			logger.Error("shutdownServer Socket creation failed")
			return EZMQ_ERROR
		}
		err = subInstance.shutdownServer.Bind(address)
		if nil != err {
			logger.Error("Error while binding shutdownServer")
			subInstance.shutdownServer = nil
			return EZMQ_ERROR
		}
	}

	if nil == subInstance.shutdownClient {
		subInstance.shutdownClient, err = zmq.NewSocket(zmq.PAIR)
		if nil != err {
			logger.Error("shutdownClient Socket creation failed")
			return EZMQ_ERROR
		}
		err = subInstance.shutdownClient.Connect(address)
		if nil != err {
			logger.Error("shutdownClient Socket connect failed")
			return EZMQ_ERROR
		}
		logger.Debug("shutdownClient subscriber", zap.String("Address", address))
	}

	if nil == subInstance.subscriber {
		subInstance.subscriber, err = zmq.NewSocket(zmq.SUB)
		if nil != err {
			logger.Error("Subscriber Socket creation failed")
			return EZMQ_ERROR
		}
		address = getSubSocketAddress(subInstance.ip, subInstance.port)
		err = subInstance.subscriber.Connect(address)
		if nil != err {
			logger.Error("Subscriber Socket connect failed")
			return EZMQ_ERROR
		}
		logger.Debug("Starting subscriber", zap.String("Address", address))
	}

	if nil == subInstance.poller {
		subInstance.poller = zmq.NewPoller()
		subInstance.poller.Add(subInstance.subscriber, zmq.POLLIN)
		subInstance.poller.Add(subInstance.shutdownClient, zmq.POLLIN)
	}

	//call a go routine [new thread] for receiver
	if false == subInstance.isReceiverStarted {
		subInstance.isReceiverStarted = true
		go receive(subInstance)
	}
	return EZMQ_OK
}

func (subInstance *EZMQSubscriber) subscribeInternal(topic string) EZMQErrorCode {
	if nil != subInstance.subscriber {
		err := subInstance.subscriber.SetSubscribe(topic)
		if nil != err {
			logger.Error("subscribeInternal error occured")
			return EZMQ_ERROR
		}
	} else {
		logger.Error("subscriber is null")
		return EZMQ_ERROR
	}
	logger.Debug("subscribed for events")
	return EZMQ_OK
}

// Subscribe for event/messages.
func (subInstance *EZMQSubscriber) Subscribe() EZMQErrorCode {
	return subInstance.subscribeInternal("")
}

// Subscribe for event/messages on a particular topic.
func (subInstance *EZMQSubscriber) SubscribeForTopic(topic string) EZMQErrorCode {
	//validate the topic
	validTopic := sanitizeTopic(topic)
	if validTopic == "" {
		return EZMQ_INVALID_TOPIC
	}
	logger.Debug("subscribing for events", zap.String("Topic", validTopic))
	return subInstance.subscribeInternal(validTopic)
}

// Subscribe for event/messages on given list of topics. On any of the topic
// in list, if it failed to subscribe events it will return
// EZMQ_ERROR/EZMQ_INVALID_TOPIC.
//
// Note:
// (1) Topic name should be as path format. For example:home/livingroom/
// (2) Topic name can have letters [a-z, A-z], numerics [0-9] and special characters _ - / and .
func (subInstance *EZMQSubscriber) SubscribeForTopicList(topicList List.List) EZMQErrorCode {
	if topicList.Len() == 0 {
		return EZMQ_INVALID_TOPIC
	}
	for topic := topicList.Front(); topic != nil; topic = topic.Next() {
		result := subInstance.SubscribeForTopic(topic.Value.(string))
		if result != EZMQ_OK {
			return result
		}
	}
	return EZMQ_OK
}

func (subInstance *EZMQSubscriber) unSubscribeInternal(topic string) EZMQErrorCode {
	if nil != subInstance.subscriber {
		err := subInstance.subscriber.SetUnsubscribe(topic)
		if nil != err {
			logger.Error("subscriber is null")
			return EZMQ_ERROR
		}
	} else {
		return EZMQ_ERROR
	}
	return EZMQ_OK
}

// Un-subscribe all the events from publisher.
func (subInstance *EZMQSubscriber) UnSubscribe() EZMQErrorCode {
	return subInstance.unSubscribeInternal("")
}

// Un-subscribe specific topic events.
//
// Note:
// (1) Topic name should be as path format. For example:home/livingroom/
// (2) Topic name can have letters [a-z, A-z], numerics [0-9] and special characters _ - / and .
func (subInstance *EZMQSubscriber) UnSubscribeForTopic(topic string) EZMQErrorCode {
	//validate the topic
	validTopic := sanitizeTopic(topic)
	if validTopic == "" {
		return EZMQ_INVALID_TOPIC
	}
	logger.Debug("Unsubscribe for events", zap.String("Topic", validTopic))
	return subInstance.unSubscribeInternal(validTopic)
}

// Un-subscribe event/messages on given list of topics. On any of the topic
// in list, if it failed to unsubscribe events it will return
// EZMQ_ERROR/EZMQ_INVALID_TOPIC.
//
// Note:
// (1) Topic name should be as path format. For example:home/livingroom/ .
// (2) Topic name can have letters [a-z, A-z], numerics [0-9] and special characters _ - / and .
func (subInstance *EZMQSubscriber) UnSubscribeForTopicList(topicList List.List) EZMQErrorCode {
	if topicList.Len() == 0 {
		return EZMQ_INVALID_TOPIC
	}
	for topic := topicList.Front(); topic != nil; topic = topic.Next() {
		result := subInstance.UnSubscribeForTopic(topic.Value.(string))
		if result != EZMQ_OK {
			return result
		}
	}
	return EZMQ_OK
}

// Stops SUB instance.
func (subInstance *EZMQSubscriber) Stop() EZMQErrorCode {
	if nil != subInstance.shutdownServer && subInstance.isReceiverStarted == true {
		subInstance.shutdownChan = make(chan string)
		timeout := make(chan bool, 1)
		go func() {
			time.Sleep(1 * time.Second)
			timeout <- true
		}()
		result, err := subInstance.shutdownServer.Send("shutdown", 0)
		if nil != err {
			logger.Error("Error while sending event on shutdownServer", zap.Int("result: ", result))
		} else {
			select {
			case <-subInstance.shutdownChan:
				logger.Debug("Received success shutdown signal")
			case <-timeout:
				logger.Debug("Timeout occured for shutdown socket")
			}
		}
	}

	if nil != subInstance.poller {
		subInstance.poller.RemoveBySocket(subInstance.subscriber)
		subInstance.poller.RemoveBySocket(subInstance.shutdownClient)
	}

	if nil != subInstance.shutdownClient {
		err := subInstance.shutdownClient.Close()
		if nil != err {
			logger.Error("Error while closing shutdownClient socket")
			return EZMQ_ERROR
		}
	}

	if nil != subInstance.shutdownServer {
		err := subInstance.shutdownServer.Close()
		if nil != err {
			logger.Error("Error while closing shutdownServer socket")
			return EZMQ_ERROR
		}
	}

	if nil != subInstance.subscriber {
		err := subInstance.subscriber.Close()
		if nil != err {
			logger.Error("Error while closing subscriber")
			return EZMQ_ERROR
		}
	}

	subInstance.poller = nil
	subInstance.shutdownClient = nil
	subInstance.shutdownServer = nil
	subInstance.subscriber = nil
	subInstance.shutdownChan = nil
	subInstance.isReceiverStarted = false
	logger.Debug("Subscriber stopped")
	return EZMQ_OK
}

// Get Ip of publisher to which subscribed.
func (subInstance *EZMQSubscriber) GetIP() string {
	return subInstance.ip
}

// Get Port of publisher to which subscribed.
func (subInstance *EZMQSubscriber) GetPort() int {
	return subInstance.port
}

func getSubSocketAddress(ip string, port int) string {
	return string(SUB_TCP_PREFIX) + ip + ":" + strconv.Itoa(port)
}

func getInProcUniqueAddress() string {
	return string(INPROC_PREFIX) + strconv.Itoa(rand.Intn(10000000))
}
