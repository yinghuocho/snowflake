package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"time"

	"github.com/dchest/uniuri"
	"github.com/keroserene/go-webrtc"
)

// Remote WebRTC peer.
// Implements the |Snowflake| interface, which includes
// |io.ReadWriter|, |Resetter|, and |Connector|.
//
// Handles preparation of go-webrtc PeerConnection. Only ever has
// one DataChannel.
type WebRTCPeer struct {
	id        string
	config    *webrtc.Configuration
	pc        *webrtc.PeerConnection
	transport SnowflakeDataChannel // Holds the WebRTC DataChannel.
	broker    *BrokerChannel

	offerChannel  chan *webrtc.SessionDescription
	answerChannel chan *webrtc.SessionDescription
	errorChannel  chan error
	recvPipe      *io.PipeReader
	writePipe     *io.PipeWriter
	lastReceive   time.Time
	buffer        bytes.Buffer
	reset         chan struct{}

	closed bool

	BytesLogger
}

// Construct a WebRTC PeerConnection.
func NewWebRTCPeer(config *webrtc.Configuration,
	broker *BrokerChannel) *WebRTCPeer {
	connection := new(WebRTCPeer)
	connection.id = "snowflake-" + uniuri.New()
	connection.config = config
	connection.broker = broker
	connection.offerChannel = make(chan *webrtc.SessionDescription, 1)
	connection.answerChannel = make(chan *webrtc.SessionDescription, 1)
	// Error channel is mostly for reporting during the initial SDP offer
	// creation & local description setting, which happens asynchronously.
	connection.errorChannel = make(chan error, 1)
	connection.reset = make(chan struct{}, 1)

	// Override with something that's not NullLogger to have real logging.
	connection.BytesLogger = &BytesNullLogger{}

	// Pipes remain the same even when DataChannel gets switched.
	connection.recvPipe, connection.writePipe = io.Pipe()
	return connection
}

// Read bytes from local SOCKS.
// As part of |io.ReadWriter|
func (c *WebRTCPeer) Read(b []byte) (int, error) {
	return c.recvPipe.Read(b)
}

// Writes bytes out to remote WebRTC.
// As part of |io.ReadWriter|
func (c *WebRTCPeer) Write(b []byte) (int, error) {
	c.BytesLogger.AddOutbound(len(b))
	// TODO: Buffering could be improved / separated out of WebRTCPeer.
	if nil == c.transport {
		log.Printf("Buffered %d bytes --> WebRTC", len(b))
		c.buffer.Write(b)
	} else {
		c.transport.Send(b)
	}
	return len(b), nil
}

// As part of |Snowflake|
func (c *WebRTCPeer) Close() error {
	if c.closed { // Skip if already closed.
		return nil
	}
	// Mark for deletion.
	c.closed = true
	c.cleanup()
	c.Reset()
	log.Printf("WebRTC: Closing")
	return nil
}

// As part of |Resetter|
func (c *WebRTCPeer) Reset() {
	if nil == c.reset {
		return
	}
	c.reset <- struct{}{}
}

// As part of |Resetter|
func (c *WebRTCPeer) WaitForReset() { <-c.reset }

// Prevent long-lived broken remotes.
// Should also update the DataChannel in underlying go-webrtc's to make Closes
// more immediate / responsive.
func (c *WebRTCPeer) checkForStaleness() {
	c.lastReceive = time.Now()
	for {
		if c.closed {
			return
		}
		if time.Since(c.lastReceive).Seconds() > SnowflakeTimeout {
			log.Println("WebRTC: No messages received for", SnowflakeTimeout,
				"seconds -- closing stale connection.")
			c.Close()
			return
		}
		<-time.After(time.Second)
	}
}

// As part of |Connector| interface.
func (c *WebRTCPeer) Connect() error {
	log.Println(c.id, " connecting...")
	// TODO: When go-webrtc is more stable, it's possible that a new
	// PeerConnection won't need to be re-prepared each time.
	err := c.preparePeerConnection()
	if err != nil {
		return err
	}
	err = c.establishDataChannel()
	if err != nil {
		return errors.New("WebRTC: Could not establish DataChannel.")
	}
	err = c.exchangeSDP()
	if err != nil {
		return err
	}
	go c.checkForStaleness()
	return nil
}

// Create and prepare callbacks on a new WebRTC PeerConnection.
func (c *WebRTCPeer) preparePeerConnection() error {
	if nil != c.pc {
		c.pc.Close()
		c.pc = nil
	}
	pc, err := webrtc.NewPeerConnection(c.config)
	if err != nil {
		log.Printf("NewPeerConnection ERROR: %s", err)
		return err
	}
	// Prepare PeerConnection callbacks.
	pc.OnNegotiationNeeded = func() {
		log.Println("WebRTC: OnNegotiationNeeded")
		go func() {
			offer, err := pc.CreateOffer()
			// TODO: Potentially timeout and retry if ICE isn't working.
			if err != nil {
				c.errorChannel <- err
				return
			}
			err = pc.SetLocalDescription(offer)
			if err != nil {
				c.errorChannel <- err
				return
			}
		}()
	}
	// Allow candidates to accumulate until OnIceComplete.
	pc.OnIceCandidate = func(candidate webrtc.IceCandidate) {
		log.Printf(candidate.Candidate)
	}
	// TODO: This may soon be deprecated, consider OnIceGatheringStateChange.
	pc.OnIceComplete = func() {
		log.Printf("WebRTC: OnIceComplete")
		c.offerChannel <- pc.LocalDescription()
	}
	// This callback is not expected, as the Client initiates the creation
	// of the data channel, not the remote peer.
	pc.OnDataChannel = func(channel *webrtc.DataChannel) {
		log.Println("OnDataChannel")
		panic("Unexpected OnDataChannel!")
	}
	c.pc = pc
	log.Println("WebRTC: PeerConnection created.")
	return nil
}

// Create a WebRTC DataChannel locally.
func (c *WebRTCPeer) establishDataChannel() error {
	if c.transport != nil {
		panic("Unexpected datachannel already exists!")
	}
	dc, err := c.pc.CreateDataChannel(c.id, webrtc.Init{})
	// Triggers "OnNegotiationNeeded" on the PeerConnection, which will prepare
	// an SDP offer while other goroutines operating on this struct handle the
	// signaling. Eventually fires "OnOpen".
	if err != nil {
		log.Printf("CreateDataChannel ERROR: %s", err)
		return err
	}
	dc.OnOpen = func() {
		log.Println("WebRTC: DataChannel.OnOpen")
		if nil != c.transport {
			panic("WebRTC: transport already exists.")
		}
		// Flush buffered outgoing SOCKS data if necessary.
		if c.buffer.Len() > 0 {
			dc.Send(c.buffer.Bytes())
			log.Println("Flushed", c.buffer.Len(), "bytes.")
			c.buffer.Reset()
		}
		// Then enable the datachannel.
		c.transport = dc
	}
	dc.OnClose = func() {
		// Future writes will go to the buffer until a new DataChannel is available.
		if nil == c.transport {
			// Closed locally, as part of a reset.
			log.Println("WebRTC: DataChannel.OnClose [locally]")
			return
		}
		// Closed remotely, need to reset everything.
		// Disable the DataChannel as a write destination.
		log.Println("WebRTC: DataChannel.OnClose [remotely]")
		c.transport = nil
		c.Close()
	}
	dc.OnMessage = func(msg []byte) {
		if len(msg) <= 0 {
			log.Println("0 length message---")
		}
		c.BytesLogger.AddInbound(len(msg))
		n, err := c.writePipe.Write(msg)
		if err != nil {
			// TODO: Maybe shouldn't actually close.
			log.Println("Error writing to SOCKS pipe")
			c.writePipe.CloseWithError(err)
		}
		if n != len(msg) {
			log.Println("Error: short write")
			panic("short write")
		}
		c.lastReceive = time.Now()
	}
	log.Println("WebRTC: DataChannel created.")
	return nil
}

func (c *WebRTCPeer) sendOfferToBroker() {
	if nil == c.broker {
		return
	}
	offer := c.pc.LocalDescription()
	answer, err := c.broker.Negotiate(offer)
	if nil != err || nil == answer {
		log.Printf("BrokerChannel Error: %s", err)
		answer = nil
	}
	c.answerChannel <- answer
}

// Block until an SDP offer is available, send it to either
// the Broker or signal pipe, then await for the SDP answer.
func (c *WebRTCPeer) exchangeSDP() error {
	select {
	case offer := <-c.offerChannel:
		// Display for copy-paste when no broker available.
		if nil == c.broker {
			log.Printf("Please Copy & Paste the following to the peer:")
			log.Printf("----------------")
			log.Printf("\n\n" + offer.Serialize() + "\n\n")
			log.Printf("----------------")
		}
	case err := <-c.errorChannel:
		log.Println("Failed to prepare offer", err)
		c.Close()
		return err
	}
	// Keep trying the same offer until a valid answer arrives.
	var ok bool
	var answer *webrtc.SessionDescription = nil
	for nil == answer {
		go c.sendOfferToBroker()
		answer, ok = <-c.answerChannel // Blocks...
		if !ok || nil == answer {
			log.Printf("Failed to retrieve answer. Retrying in %d seconds", ReconnectTimeout)
			<-time.After(time.Second * ReconnectTimeout)
			answer = nil
		}
	}
	log.Printf("Received Answer:\n\n%s\n", answer.Sdp)
	err := c.pc.SetRemoteDescription(answer)
	if nil != err {
		log.Println("WebRTC: Unable to SetRemoteDescription:", err)
		return err
	}
	return nil
}

// Close all channels and transports
func (c *WebRTCPeer) cleanup() {
	if nil != c.offerChannel {
		close(c.offerChannel)
	}
	if nil != c.answerChannel {
		close(c.answerChannel)
	}
	if nil != c.errorChannel {
		close(c.errorChannel)
	}
	// Close this side of the SOCKS pipe.
	if nil != c.writePipe {
		c.writePipe.Close()
		c.writePipe = nil
	}
	if nil != c.transport {
		log.Printf("WebRTC: closing DataChannel")
		dataChannel := c.transport
		// Setting transport to nil *before* dc Close indicates to OnClose that
		// this was locally triggered.
		c.transport = nil
		dataChannel.Close()
	}
	if nil != c.pc {
		log.Printf("WebRTC: closing PeerConnection")
		err := c.pc.Close()
		if nil != err {
			log.Printf("Error closing peerconnection...")
		}
		c.pc = nil
	}
}
