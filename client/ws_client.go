package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Callback signatures
type OrderBookCallback func(marketID string, state OrderBook)
type AccountCallback func(accountID string, state Account)

// WsClient represents a WebSocket client for order book and account updates
type WsClient struct {
	url               string
	subscriptions     subscriptions
	conn              *websocket.Conn
	orderBookStates   map[string]OrderBook
	accounts          map[string]Account
	onOrderBookUpdate OrderBookCallback
	onAccountUpdate   AccountCallback
	mu                sync.Mutex
}

type subscriptions struct {
	OrderBooks []string
	Accounts   []string
}

// OrderBook represents the order book payload
type OrderBook struct {
	Asks []Order `json:"asks"`
	Bids []Order `json:"bids"`
}

// Order represents a single order entry
type Order struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// Account represents the account payload
type Account struct {
	// Define fields based on payload structure
	Data json.RawMessage `json:"data"`
}

// message generic wrapper
type message struct {
	Type      string          `json:"type"`
	Channel   string          `json:"channel"`
	OrderBook json.RawMessage `json:"order_book"`
	// raw account payload for simplicity
	// handles both subscribed and update
	// embed full message if needed
}

// NewWsClient constructs a new WsClient
func NewWsClient(host string, path string,
	orderBookIDs, accountIDs []string,
	onOB OrderBookCallback,
	onAcct AccountCallback) (*WsClient, error) {
	if len(orderBookIDs) == 0 && len(accountIDs) == 0 {
		return nil, fmt.Errorf("no subscriptions provided")
	}
	if host == "" {
		host = "localhost:443" // default host if needed
	}
	u := url.URL{Scheme: "wss", Host: host, Path: path}
	return &WsClient{
		url:               u.String(),
		subscriptions:     subscriptions{OrderBooks: orderBookIDs, Accounts: accountIDs},
		orderBookStates:   make(map[string]OrderBook),
		accounts:          make(map[string]Account),
		onOrderBookUpdate: onOB,
		onAccountUpdate:   onAcct,
	}, nil
}

// Run connects and listens for messages
func (c *WsClient) Run() error {
	var err error
	c.conn, _, err = websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		return fmt.Errorf("dial error: %w", err)
	}
	defer c.conn.Close()

	// send initial subscriptions
	if err := c.handleConnected(); err != nil {
		return err
	}

	for {
		_, msgBytes, err := c.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message error: %w", err)
		}
		if err := c.onMessage(msgBytes); err != nil {
			log.Printf("message handling error: %v", err)
		}
	}
}

func (c *WsClient) onMessage(raw []byte) error {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("json unmarshal error: %w", err)
	}

	switch msg.Type {
	case "connected":
		// already subscribed in Run
	case "subscribed/order_book":
		return c.handleSubscribedOrderBook(msg)
	case "update/order_book":
		return c.handleUpdateOrderBook(msg)
	case "subscribed/account_all":
		return c.handleSubscribedAccount(raw)
	case "update/account_all":
		return c.handleUpdateAccount(raw)
	default:
		return fmt.Errorf("unhandled message type: %s", msg.Type)
	}
	return nil
}

func (c *WsClient) handleConnected() error {
	for _, marketID := range c.subscriptions.OrderBooks {
		req := map[string]string{"type": "subscribe", "channel": fmt.Sprintf("order_book/%s", marketID)}
		if err := c.conn.WriteJSON(req); err != nil {
			return err
		}
	}
	for _, acctID := range c.subscriptions.Accounts {
		req := map[string]string{"type": "subscribe", "channel": fmt.Sprintf("account_all/%s", acctID)}
		if err := c.conn.WriteJSON(req); err != nil {
			return err
		}
	}
	return nil
}

func (c *WsClient) handleSubscribedOrderBook(msg message) error {
	// channel format "order_book:<id>"
	parts := splitChannel(msg.Channel)
	marketID := parts[1]
	var ob OrderBook
	if err := json.Unmarshal(msg.OrderBook, &ob); err != nil {
		return err
	}
	c.mu.Lock()
	c.orderBookStates[marketID] = ob
	c.mu.Unlock()
	if c.onOrderBookUpdate != nil {
		c.onOrderBookUpdate(marketID, ob)
	}
	return nil
}

func (c *WsClient) handleUpdateOrderBook(msg message) error {
	parts := splitChannel(msg.Channel)
	marketID := parts[1]
	var delta OrderBook
	if err := json.Unmarshal(msg.OrderBook, &delta); err != nil {
		return err
	}
	c.mu.Lock()
	state := c.orderBookStates[marketID]
	state = updateOrderBookState(state, delta)
	c.orderBookStates[marketID] = state
	c.mu.Unlock()
	if c.onOrderBookUpdate != nil {
		c.onOrderBookUpdate(marketID, state)
	}
	return nil
}

func updateOrderBookState(state, delta OrderBook) OrderBook {
	state.Asks = mergeOrders(state.Asks, delta.Asks)
	state.Bids = mergeOrders(state.Bids, delta.Bids)
	return state
}

func mergeOrders(existing, updates []Order) []Order {
	m := make(map[string]string)
	for _, ord := range existing {
		m[ord.Price] = ord.Size
	}
	for _, upd := range updates {
		if upd.Size == "0" {
			// remove
			delete(m, upd.Price)
		} else {
			m[upd.Price] = upd.Size
		}
	}
	var merged []Order
	for price, size := range m {
		merged = append(merged, Order{Price: price, Size: size})
	}
	return merged
}

func (c *WsClient) handleSubscribedAccount(raw []byte) error {
	// reuse message for full account state
	var msg map[string]interface{}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	channel := msg["channel"].(string)
	parts := splitChannel(channel)
	acctID := parts[1]
	state := Account{Data: raw}
	c.mu.Lock()
	c.accounts[acctID] = state
	c.mu.Unlock()
	if c.onAccountUpdate != nil {
		c.onAccountUpdate(acctID, state)
	}
	return nil
}

func (c *WsClient) handleUpdateAccount(raw []byte) error {
	return c.handleSubscribedAccount(raw)
}

func splitChannel(channel string) []string {
	// splits "order_book:<id>" or "account_all:<id>"
	if strings.Contains(channel, ":") {
		return strings.SplitN(channel, ":", 2)
	} else if strings.Contains(channel, "/") {
		return strings.SplitN(channel, "/", 2)
	}
	return []string{channel, ""}
}

func indexOf(s, sep string) int {
	return strings.Index(s, sep)
}
