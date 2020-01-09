package session

import (
	"regexp"
	"strings"
	"sync"

	"github.com/baetyl/baetyl-broker/common"
	"github.com/baetyl/baetyl-go/link"
	"github.com/baetyl/baetyl-go/log"
	"github.com/baetyl/baetyl-go/mqtt"
	"github.com/baetyl/baetyl-go/utils"
	"github.com/docker/distribution/uuid"
)

// ClientMQTT the client of MQTT
type ClientMQTT struct {
	id         string
	manager    *Manager
	session    *Session
	resender   *resender
	counter    *mqtt.Counter
	authorizer *Authorizer
	connection mqtt.Connection

	log  *log.Logger
	tomb utils.Tomb
	sync.Mutex
	sync.Once
}

func (m *Manager) initClientMQTT(connection mqtt.Connection) {
	id := strings.ReplaceAll(uuid.Generate().String(), "-", "")
	c := &ClientMQTT{
		id:         id,
		manager:    m,
		connection: connection,
		counter:    mqtt.NewCounter(),
		log:        log.With(log.Any("type", "mqtt"), log.Any("id", id)),
	}
	c.resender = newResender(m.cfg.MaxInflightQOS1Messages, m.cfg.RepublishInterval, &c.tomb)
	m.addClient(c)
	c.tomb.Go(c.receiving)
}

func (c *ClientMQTT) getID() string {
	return c.id
}

func (c *ClientMQTT) setSession(s *Session) {
	c.session = s
	c.log = c.log.With(log.Any("sid", s.ID))
}

func (c *ClientMQTT) getSession() *Session {
	return c.session
}

// Close closes client by session
func (c *ClientMQTT) Close() error {
	c.log.Info("client is closing by session")
	defer c.log.Info("client has closed by session")
	return c.close()
}

// closes client by itself
func (c *ClientMQTT) die(msg string, err error) {
	if !c.tomb.Alive() {
		return
	}
	if err != nil {
		c.log.Error(msg, log.Error(err))
	}
	go func() {
		c.log.Info("client is closing by itself")
		if err != nil {
			c.sendWillMessage()
		}
		c.manager.delClient(c)
		c.close()
		c.log.Info("client has closed by itself")
	}()
}

func (c *ClientMQTT) close() error {
	c.Do(func() {
		c.tomb.Kill(nil)
		c.connection.Close()
	})
	return c.tomb.Wait()
}

func (c *ClientMQTT) authorize(action, topic string) bool {
	return c.authorizer == nil || c.authorizer.Authorize(action, topic)
}

// SendWillMessage sends will message
func (c *ClientMQTT) sendWillMessage() {
	if c.session == nil || c.session.Will == nil {
		return
	}
	msg := c.session.Will
	if msg.Retain() {
		err := c.retainMessage(msg)
		if err != nil {
			c.log.Error("failed to retain will message", log.Any("topic", msg.Context.Topic))
		}
	}
	c.manager.exchange.Route(msg, c.callback)
}

func (c *ClientMQTT) retainMessage(msg *link.Message) error {
	if len(msg.Content) == 0 {
		return c.manager.removeRetain(msg.Context.Topic)
	}
	return c.manager.setRetain(msg.Context.Topic, msg)
}

// SendRetainMessage sends retain message
func (c *ClientMQTT) sendRetainMessage() error {
	if c.session == nil {
		return nil
	}
	msgs, err := c.manager.getRetain()
	if err != nil || len(msgs) == 0 {
		return err
	}
	for _, msg := range msgs {
		if ok, qos := mqtt.MatchTopicQOS(c.session.subs, msg.Context.Topic); ok {
			if msg.Context.QOS > qos {
				msg.Context.QOS = qos
			}
			e := common.NewEvent(msg, 0, nil)
			err = c.session.Push(e)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// checkClientID checks clientID
func checkClientID(v string) bool {
	return regexpClientID.MatchString(v)
}

var regexpClientID = regexp.MustCompile("^[0-9A-Za-z_-]{0,128}$")
