package allocation

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/pion/logging"
)

// ManagerConfig a bag of config params for Manager.
type ManagerConfig struct {
	LeveledLogger      logging.LeveledLogger
	AllocatePacketConn func(network string, requestedPort int) (net.PacketConn, net.Addr, error)
	AllocateConn       func(network string, requestedPort int) (net.Listener, net.Addr, error)
}

type reservation struct {
	token string
	port  int
}

// Manager is used to hold active allocations
type Manager struct {
	lock sync.RWMutex
	log  logging.LeveledLogger

	allocations  map[string]*Allocation
	reservations []*reservation
	waitingconns map[uint32]*Allocation
	runningconns map[uint32]*Allocation

	allocatePacketConn func(network string, requestedPort int) (net.PacketConn, net.Addr, error)
	allocateConn       func(network string, requestedPort int) (net.Listener, net.Addr, error)
}

// NewManager creates a new instance of Manager.
func NewManager(config ManagerConfig) (*Manager, error) {
	switch {
	case config.AllocatePacketConn == nil:
		return nil, fmt.Errorf("AllocatePacketConn must be set")
	// TCP allocations are not allowed from UDP connections
	// case config.AllocateConn == nil:
	// 	return nil, fmt.Errorf("AllocateConn must be set")
	case config.LeveledLogger == nil:
		return nil, fmt.Errorf("LeveledLogger must be set")
	}

	return &Manager{
		log:                config.LeveledLogger,
		allocations:        make(map[string]*Allocation, 64),
		allocatePacketConn: config.AllocatePacketConn,
		allocateConn:       config.AllocateConn,
	}, nil
}

// GetAllocation fetches the allocation matching the passed FiveTuple
func (m *Manager) GetAllocation(fiveTuple *FiveTuple) *Allocation {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.allocations[fiveTuple.Fingerprint()]
}

// Close closes the manager and closes all allocations it manages
func (m *Manager) Close() error {
	m.lock.Lock()
	defer m.lock.Unlock()

	for _, a := range m.allocations {
		if err := a.Close(); err != nil {
			return err
		}
	}
	return nil
}

// CreateAllocation creates a new allocation and starts relaying
func (m *Manager) CreateAllocation(fiveTuple *FiveTuple, turnSocket net.PacketConn, requestedPort int, lifetime time.Duration) (*Allocation, error) {
	switch {
	case fiveTuple == nil:
		return nil, fmt.Errorf("allocations must not be created with nil FivTuple")
	case fiveTuple.SrcAddr == nil:
		return nil, fmt.Errorf("allocations must not be created with nil FiveTuple.SrcAddr")
	case fiveTuple.DstAddr == nil:
		return nil, fmt.Errorf("allocations must not be created with nil FiveTuple.DstAddr")
	case turnSocket == nil:
		return nil, fmt.Errorf("allocations must not be created with nil turnSocket")
	case lifetime == 0:
		return nil, fmt.Errorf("allocations must not be created with a lifetime of 0")
	}

	if a := m.GetAllocation(fiveTuple); a != nil {
		return nil, fmt.Errorf("allocation attempt created with duplicate FiveTuple %v", fiveTuple)
	}
	a := NewAllocation(turnSocket, fiveTuple, m.log)

	switch fiveTuple.Protocol {
	case UDP:
		conn, relayAddr, err := m.allocatePacketConn("udp4", requestedPort)
		if err != nil {
			return nil, err
		}

		a.RelaySocket = conn
		a.RelayAddr = relayAddr

		m.log.Debugf("listening on relay addr: %s", a.RelayAddr.String())

		a.lifetimeTimer = time.AfterFunc(lifetime, func() {
			m.DeleteAllocation(a.fiveTuple)
		})

		m.lock.Lock()
		m.allocations[fiveTuple.Fingerprint()] = a
		m.lock.Unlock()

		go a.packetHandler(m)
	case TCP:
		listener, relayAddr, err := m.allocateConn("tcp4", requestedPort)
		if err != nil {
			return nil, err
		}

		a.RelayListener = listener
		a.RelayAddr = relayAddr

		m.log.Debugf("listening on relay addr: %s", a.RelayAddr.String())

		a.lifetimeTimer = time.AfterFunc(lifetime, func() {
			m.DeleteAllocation(a.fiveTuple)
		})

		m.lock.Lock()
		m.allocations[fiveTuple.Fingerprint()] = a
		m.lock.Unlock()

		go a.listenHandler(m)
	}

	return a, nil
}

// DeleteAllocation removes an allocation
func (m *Manager) DeleteAllocation(fiveTuple *FiveTuple) {
	fingerprint := fiveTuple.Fingerprint()

	m.lock.Lock()
	allocation := m.allocations[fingerprint]
	delete(m.allocations, fingerprint)
	m.lock.Unlock()

	if allocation == nil {
		return
	}

	if err := allocation.Close(); err != nil {
		m.log.Errorf("Failed to close allocation: %v", err)
	}
}

// CreateReservation stores the reservation for the token+port
func (m *Manager) CreateReservation(reservationToken string, port int) {
	time.AfterFunc(30*time.Second, func() {
		m.lock.Lock()
		defer m.lock.Unlock()
		for i := len(m.reservations) - 1; i >= 0; i-- {
			if m.reservations[i].token == reservationToken {
				m.reservations = append(m.reservations[:i], m.reservations[i+1:]...)
				return
			}
		}
	})

	m.lock.Lock()
	m.reservations = append(m.reservations, &reservation{
		token: reservationToken,
		port:  port,
	})
	m.lock.Unlock()
}

// GetReservation returns the port for a given reservation if it exists
func (m *Manager) GetReservation(reservationToken string) (int, bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	for _, r := range m.reservations {
		if r.token == reservationToken {
			return r.port, true
		}
	}
	return 0, false
}

// GetRandomEvenPort returns a random un-allocated udp4 port
func (m *Manager) GetRandomEvenPort() (int, error) {
	conn, addr, err := m.allocatePacketConn("udp4", 0)
	if err != nil {
		return 0, err
	}

	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("failed to cast net.Addr to *net.UDPAddr")
	} else if err := conn.Close(); err != nil {
		return 0, err
	} else if udpAddr.Port%2 == 1 {
		return m.GetRandomEvenPort()
	}

	return udpAddr.Port, nil
}

func (m *Manager) BindConnection(cid uint32) net.Conn {
	m.lock.RLock()
	defer m.lock.RUnlock()
	a := m.waitingconns[cid]
	delete(m.waitingconns, cid)
	if a == nil {
		return nil
	}
	m.runningconns[cid] = a
	return a.GetConnectionByID(cid)
}

func (m *Manager) Connect(a *Allocation, dst string) (uint32, error) {
	cid := m.newCID(a)

	err := a.connect(cid, dst)
	if err != nil {
		return 0, err
	}

	// If no ConnectionBind request associated with this peer data
	// connection is received after 30 seconds, the peer data connection
	// MUST be closed.
	go m.removeAfter30(cid, dst)

	return cid, nil
}

func (m *Manager) removeAfter30(cid uint32, dst string) {
	<-time.After(30 * time.Second)
	m.lock.Lock()
	defer m.lock.Unlock()
	a, ok := m.waitingconns[cid]
	if !ok {
		return
	}
	delete(m.waitingconns, cid)
	a.removeConnection(cid, dst)
}

func (m *Manager) newCID(a *Allocation) uint32 {
	m.lock.Lock()
	var cid uint32
	for {
		cid = rand.Uint32()
		if cid == 0 {
			continue
		} else if _, ok := m.waitingconns[cid]; ok {
			continue
		} else if _, ok := m.runningconns[cid]; ok {
			continue
		} else {
			break
		}
	}
	m.waitingconns[cid] = a
	m.lock.Unlock()

	return cid
}
