// Copyright (c) 2018, The Decred developers
// See LICENSE for details.

package insight

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/decred/dcrd/chaincfg/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types"
	"github.com/decred/dcrd/txscript/v2"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrdata/blockdata/v4"
	"github.com/decred/dcrdata/txhelpers/v3"
	engineio "github.com/googollee/go-engine.io"
	"github.com/googollee/go-engine.io/transport"
	"github.com/googollee/go-engine.io/transport/websocket"
	socketio "github.com/googollee/go-socket.io"
)

var isAlphaNumeric = regexp.MustCompile(`^[a-zA-Z0-9]+$`).MatchString

type roomSubscriptionCounter struct {
	*sync.RWMutex
	c map[string]int
}

// SocketServer wraps the socket.io server with the watched address list.
type SocketServer struct {
	socketio.Server
	params           *chaincfg.Params
	watchedAddresses roomSubscriptionCounter
	txGetter         txhelpers.RawTransactionGetter
}

// InsightSocketVin represents a single vin for the Insight "vin" JSON object
// that appears in a "tx" message from the "inv" room.
type InsightSocketVin struct {
	TxID      string   `json:"txid,omitempty"`
	Vout      *uint32  `json:"vout,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	Value     *int64   `json:"value,omitempty"`
}

func newInt64Ptr(i int64) *int64 {
	ii := i
	return &ii
}

func newUint32Ptr(i uint32) *uint32 {
	ii := i
	return &ii
}

// InsightSocketVout represents a single vout for the Insight "vout" JSON object
// that appears in a "tx" message from the "inv" room.
type InsightSocketVout struct {
	Address string
	Value   int64
}

// MarshalJSON implements json.Marshaler so that an InsightSocketVout will
// marshal to JSON like:
//	{
//	  "DsZQaCQES5vh3JmcyyFokJYz3aSw8Sm1dsQ": 13741789
//	}
func (v *InsightSocketVout) MarshalJSON() ([]byte, error) {
	vout := map[string]int64{
		v.Address: v.Value,
	}
	return json.Marshal(vout)
}

// WebSocketTx models the JSON data sent as the tx event in the inv room.
type WebSocketTx struct {
	Hash     string              `json:"txid"`
	Size     int                 `json:"size"`
	TotalOut int64               `json:"valueOut"`
	Vins     []InsightSocketVin  `json:"vins,omitempty"`
	Vouts    []InsightSocketVout `json:"vout,omitempty"`
}

// NewSocketServer constructs a new SocketServer, registering handlers for the
// "connection", "disconnection", and "subscribe" events.
func NewSocketServer(params *chaincfg.Params, txGetter txhelpers.RawTransactionGetter) (*SocketServer, error) {
	wsTrans := &websocket.Transport{
		// Without this affirmative CheckOrigin, gorilla's "sensible default" is
		// to ensure same origin.
		CheckOrigin: func(req *http.Request) bool {
			return true
		},
	}
	opts := &engineio.Options{
		PingInterval: 3 * time.Second,
		PingTimeout:  5 * time.Second,
		Transports:   []transport.Transport{wsTrans},
	}
	server, err := socketio.NewServer(opts)
	if err != nil {
		apiLog.Errorf("Could not create socket.io server: %v", err)
		return nil, err
	}

	// Set maximum number of connections.
	//server.SetMaxConnection(16384)

	// Each address subscription uses its own room, which has the same name as
	// the address. The number of subscribers for each room is tracked.
	addrs := roomSubscriptionCounter{
		RWMutex: new(sync.RWMutex),
		c:       make(map[string]int),
	}

	server.OnConnect("/", func(so socketio.Conn) error {
		// New connections automatically join the inv and sync rooms.
		so.Join("inv")
		so.Join("sync")
		apiLog.Debugf("New socket.io connection. %d clients are connected.",
			server.RoomLen("inv"))
		return nil
	})

	// Subscription to a room checks the room name is as expected for an
	// address, joins the room, and increments the room's subscriber count.
	server.OnEvent("/", "subscribe", func(so socketio.Conn, room string) {
		if len(room) > 64 || !isAlphaNumeric(room) {
			return
		}
		if _, err := dcrutil.DecodeAddress(room, params); err == nil {
			so.Join(room)
			apiLog.Debugf("socket.io client joining room: %s", room)

			addrs.Lock()
			addrs.c[room]++
			addrs.Unlock()
		}
	})

	// Disconnection decrements or deletes the subscriber counter for each
	// address room to which the client was subscribed.
	server.OnDisconnect("/", func(s socketio.Conn, msg string) {
		apiLog.Debugf("socket.io client disconnected. %d clients are connected. msg: %s",
			server.RoomLen("inv"), msg)
		addrs.Lock()
		for _, str := range s.Rooms() {
			if c, ok := addrs.c[str]; ok {
				if c == 1 {
					delete(addrs.c, str)
				} else {
					addrs.c[str]--
				}
			}
		}
		addrs.Unlock()
	})

	server.OnError("/", func(err error) {
		apiLog.Errorf("Insight socket.io server error: %v", err)
	})

	apiLog.Infof("Started Insight socket.io server.")

	sockServ := SocketServer{
		Server:           *server,
		params:           params,
		watchedAddresses: addrs,
		txGetter:         txGetter,
	}

	go server.Serve()
	return &sockServ, nil
}

// Store broadcasts the lastest block hash to the the inv room. The coinbase
// transaction is also relayed to the new Tx channel where it is included in tx
// and address broadcasts.
func (soc *SocketServer) Store(blockData *blockdata.BlockData, msgBlock *wire.MsgBlock) error {
	apiLog.Debugf("Sending new websocket block %s", blockData.Header.Hash)
	soc.BroadcastToRoom("inv", "block", blockData.Header.Hash)

	// Since the coinbase transaction is generated by the miner, it will never
	// hit mempool. It must be processed now, with the new block.
	return soc.sendNewMsgTx(msgBlock.Transactions[0])
}

// SendNewTx prepares a dcrd mempool tx for broadcast. This method satisfies
// notification.TxHandler and is registered as a handler in main.go.
func (soc *SocketServer) SendNewTx(rawTx *chainjson.TxRawResult) error {
	msgTx, err := txhelpers.MsgTxFromHex(rawTx.Hex)
	if err != nil {
		return err
	}
	return soc.sendNewTx(msgTx, rawTx.Vout)
}

// sendNewMsgTx processes and broadcasts a msgTx to subscribers.
func (soc *SocketServer) sendNewMsgTx(msgTx *wire.MsgTx) error {
	return soc.sendNewTx(msgTx, nil)
}

// sendNewTx processes and broadcasts a msgTx to subscribers, using an existing
// []Vout, if it is available. If vouts is zero-length, the output addresses are
// decoded from their pkScripts.
func (soc *SocketServer) sendNewTx(msgTx *wire.MsgTx, vouts []chainjson.Vout) error {
	var vins []InsightSocketVin
	for _, v := range msgTx.TxIn {
		txid := v.PreviousOutPoint.Hash.String()
		idx := v.PreviousOutPoint.Index
		tree := v.PreviousOutPoint.Tree
		var addrs []string
		var amt dcrutil.Amount
		if txhelpers.IsZeroHashStr(txid) {
			// Coinbase and stake base inputs need to be "{}".
			vins = append(vins, InsightSocketVin{})
			continue
		} else {
			var err error
			addrs, amt, err = txhelpers.OutPointAddressesFromString(
				txid, idx, tree, soc.txGetter, soc.params)
			if err != nil {
				apiLog.Warnf("failed to get outpoint address from txid: %v", err)
				// Still must append this vin to maintain valid implicit
				// indexing of vins array.
			}
		}
		vins = append(vins, InsightSocketVin{
			TxID:      txid,
			Vout:      newUint32Ptr(idx),
			Addresses: addrs,
			Value:     newInt64Ptr(int64(amt)),
		})
	}

	var voutAddrs [][]string
	for i, v := range msgTx.TxOut {
		// Allow Vouts to be nil or empty, extracting the addresses from the
		// pkScripts here.
		if len(vouts) == 0 {
			_, scriptAddrs, _, err := txscript.ExtractPkScriptAddrs(
				v.Version, v.PkScript, soc.params)
			if err != nil {
				apiLog.Warnf("failed to decode pkScript: %v", err)
				// still need to append an empty array to voutAddrs.
			}
			var addrs []string
			for i := range scriptAddrs {
				addrs = append(addrs, scriptAddrs[i].Address())
			}
			voutAddrs = append(voutAddrs, addrs)
		} else {
			voutAddrs = append(voutAddrs, vouts[i].ScriptPubKey.Addresses)
		}
	}

	hash := msgTx.TxHash().String()
	var voutsInsight []InsightSocketVout
	var total int64
	for i, v := range msgTx.TxOut {
		total += v.Value
		if len(voutAddrs[i]) == 0 {
			continue
		}

		soc.watchedAddresses.RLock()
		for _, address := range voutAddrs[i] {
			if _, ok := soc.watchedAddresses.c[address]; ok {
				soc.BroadcastToRoom(address, address, hash)
			}
			voutsInsight = append(voutsInsight, InsightSocketVout{
				Address: address,
				Value:   v.Value,
			})
		}
		soc.watchedAddresses.RUnlock()
	}

	tx := WebSocketTx{
		Hash:     hash,
		Size:     msgTx.SerializeSize(),
		TotalOut: total,
		Vins:     vins,
		Vouts:    voutsInsight,
	}
	apiLog.Tracef("Sending new websocket tx %s", hash)
	soc.BroadcastToRoom("inv", "tx", tx)
	return nil
}
