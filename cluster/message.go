package cluster

import (
	"encoding/json"

	"github.com/eleme/banshee/models"
	"github.com/eleme/banshee/storage"
	"github.com/eleme/banshee/util/log"
	"github.com/streadway/amqp"
)

// Rule types
const (
	RULEADD    = "add"
	RULEDELETE = "delete"
)

// Exchanges
const (
	ExchangeType = "fanout"
)

const bufferedChangedRulesLimit = 128

type message struct {
	Type string       `json:"type"`
	Rule *models.Rule `json:"rule"`
}

// Options for message hub.
type Options struct {
	Master       bool   `json:"master"`
	DSN          string `json:"dsn"`
	VHost        string `json:"vHost"`
	ExchangeName string `json:"exchangeName"`
	QueueName    string `json:"queueName"`
}

// Hub is the message hub for rule changes.
type Hub struct {
	opts      *Options
	db        *storage.DB
	conn      *amqp.Connection
	msgCh     chan *message
	addRuleCh chan *models.Rule
	delRuleCh chan *models.Rule
}

// New create a  Hub.
func New(opts *Options, db *storage.DB) (*Hub, error) {
	conn, err := amqp.Dial(opts.DSN)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		opts:      opts,
		db:        db,
		conn:      conn,
		msgCh:     make(chan *message, bufferedChangedRulesLimit*2),
		addRuleCh: make(chan *models.Rule, bufferedChangedRulesLimit),
		delRuleCh: make(chan *models.Rule, bufferedChangedRulesLimit),
	}
	errCh := make(chan error, 1)
	if opts.Master {
		h.initRuleListener()
		go h.publisherW(errCh)
	} else {
		go h.consumerW(errCh)
	}
	err = <-errCh
	if err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

func (h *Hub) initRuleListener() {
	h.db.Admin.RulesCache.OnAdd(h.addRuleCh)
	h.db.Admin.RulesCache.OnDel(h.delRuleCh)
	h.initAddRuleListener()
	h.initDelRuleListener()
}

func (h *Hub) initAddRuleListener() {
	go func() {
		for {
			rule := <-h.addRuleCh
			h.msgCh <- &message{Type: RULEADD, Rule: rule}
		}
	}()
}

func (h *Hub) initDelRuleListener() {
	go func() {
		for {
			rule := <-h.delRuleCh
			h.msgCh <- &message{Type: RULEDELETE, Rule: rule}
		}
	}()
}

func (h *Hub) publisherW(errCh chan error) {
	ch, err := h.conn.Channel()
	if err != nil {
		errCh <- err
		return
	}
	defer ch.Close()
	err = ch.ExchangeDeclare(h.opts.ExchangeName, ExchangeType, false, false, false, false, nil)
	if err != nil {
		errCh <- err
		return
	}
	errCh <- nil
	for msg := range h.msgCh {
		buf, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		err = ch.Publish(h.opts.ExchangeName, "", false, false, amqp.Publishing{
			ContentType: "text/plain",
			Body:        buf,
		})
		log.Infof("sending rule message %v", msg)
	}
	return
}

func (h *Hub) consumerW(errCh chan error) {
	ch, err := h.conn.Channel()
	if err != nil {
		errCh <- err
		return
	}
	defer ch.Close()
	err = ch.ExchangeDeclare(h.opts.ExchangeName, ExchangeType, false, false, false, false, nil)
	if err != nil {
		errCh <- err
		return
	}
	q, err := ch.QueueDeclare(h.opts.QueueName, false, false, false, false, nil)
	if err != nil {
		errCh <- err
		return
	}
	err = ch.QueueBind(q.Name, "", h.opts.ExchangeName, false, nil)
	if err != nil {
		errCh <- err
		return
	}
	msgs, err := ch.Consume(q.Name, "", true, false, false, false, nil)
	if err != nil {
		errCh <- err
		return
	}
	errCh <- nil
	for msg := range msgs {
		var m message
		err := json.Unmarshal(msg.Body, &m)
		if err != nil {
			continue
		}
		if m.Rule == nil {
			continue
		}
		log.Infof("received message %v", m)
		if m.Type == RULEADD {
			h.db.Admin.RulesCache.Put(m.Rule)
		} else if m.Type == RULEDELETE {
			h.db.Admin.RulesCache.Delete(m.Rule.ID)
		}
	}
}

// Close close message hub.
func (h *Hub) Close() {
	h.conn.Close()
}
