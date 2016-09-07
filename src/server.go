/*
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 * Author: FTwOoO <booobooob@gmail.com>
 */

package hop

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"github.com/FTwOoO/water"
	"github.com/FTwOoO/water/waterutil"
	"github.com/FTwOoO/go-logger"
	"./conn"
)

const (
	IFACE_BUFSIZE = 2000
	BUF_SIZE = 2048
)

var log logger.Logger

type CandyVPNServer struct {
	// config
	cfg       CandyVPNServerConfig

	peers     *VPNPeers

	// interface
	iface     *water.Interface

	// channel to put in packets read from udpsocket
	fromNet   chan *RawPacket

	// channel to put packets to send through udpsocket
	toNet     chan *RawPacket

	// channel to put frames read from tun/tap device
	fromIface chan []byte

	// channel to put frames to send to tun/tap device
	toIface   chan *HopPacket

	pktHandle map[byte](func(*VPNPeer, *HopPacket))

	_lock     sync.RWMutex
}

func NewServer(cfg CandyVPNServerConfig) (err error) {

	log, err := logger.NewLogger(cfg.LogFile, cfg.LogLevel)
	if err != nil {
		return
	}

	//cipher, err = enc.NewSalsa20BlockCrypt([]byte(cfg.Key))
	//if err != nil {
	//	return err
	//}

	hopServer := new(CandyVPNServer)
	hopServer.fromNet = make(chan *RawPacket, BUF_SIZE)
	hopServer.fromIface = make(chan []byte, BUF_SIZE)
	hopServer.toIface = make(chan *HopPacket, BUF_SIZE * 4)
	hopServer.peers = make(map[uint64]*VPNPeer)
	hopServer.cfg = cfg
	hopServer.toNet = make(chan *RawPacket, BUF_SIZE)

	iface, err := water.NewTUN("tun1")
	if err != nil {
		return err
	}

	hopServer.iface = iface
	ip, subnet, err := net.ParseCIDR(cfg.Subnet)
	err = iface.SetupNetwork(ip, *subnet, cfg.MTU)
	if err != nil {
		log.Error(err.Error())
		return err
	}

	err = iface.SetupNATForServer()
	if err != nil {
		log.Error(err.Error())
		return err
	}

	hopServer.peers = NewVPNPeers(subnet, hopServer.cfg.PeerTimeout)


	//读取Tun并转发:
	//[Server Tun] —> [fromIface buf] —> [toNet buf]—>udp—> [Client]

	//接收客户端节点的Data类型协议包并转发:
	//[Client] —>udp—> [fromNet buf] —> [toIface buf] —> [Server Tun]

	//接收客户端节控制类型协议包并回复:
	//[Client] —>udp—> [fromNet buf] —>udp—> [Client]


	for port := cfg.PortStart; port <= cfg.PortEnd; port++ {
		go hopServer.listen(conn.PROTO_KCP, fmt.Sprintf("%s:%d", cfg.ListenAddr, port))
	}

	go hopServer.handleInterface()
	go hopServer.forwardFrames()

	go hopServer.peerTimeoutWatcher()
	hopServer.cleanUp()
}

func (srv *CandyVPNServer) handleInterface() {
	go func() {
		for {
			p := <-srv.toIface
			log.Debug("New Net packet to device")
			_, err := srv.iface.Write(p.))
			if err != nil {
				return
			}
		}
	}()

	buf := make([]byte, IFACE_BUFSIZE)
	for {
		n, err := srv.iface.Read(buf)
		if err != nil {
			log.Error(err.Error())
			return
		}
		hpbuf := make([]byte, n)
		copy(hpbuf, buf)
		log.Debug("New Net packet from device")
		srv.fromIface <- hpbuf
	}
}

func (srv *CandyVPNServer) listen(protocol conn.TransProtocol, addr string) {
	l, err := conn.Listen(protocol, addr)
	if err != nil {
		log.Errorf("Failed to listen on %s: %s", addr, err.Error())
		return
	}

	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Errorf("Server %d close because of %v", addr, err)
			return
		}

		go func(conn *net.Conn) {
			for {
				var plen int
				buf := make([]byte, IFACE_BUFSIZE)
				plen, err = conn.Read(buf)
				if err != nil {
					log.Error(err.Error())
					return
				}

				packet := RawPacket{data:buf[:plen], conn:conn}
				srv.fromNet <- packet
			}
		}(conn)
	}
}

func (srv *CandyVPNServer) forwardFrames() {

	srv.pktHandle = map[byte](func(*VPNPeer, *HopPacket)){
		HOP_FLG_PING:               srv.handlePing,
		HOP_FLG_PING_ACK: nil,
		HOP_FLG_HSH:               srv.handleHandshake,
		HOP_FLG_HSH_ACK: srv.handleHandshakeAck,
		HOP_FLG_DAT:               srv.handleDataPacket,
		HOP_FLG_FIN:               srv.handleFinish,
	}

	go func() {
		for {
			select {
			case packet := <-srv.toNet:
				packet.Send()
			}
		}
	}()

	for {
		select {
		case pack := <-srv.fromIface:
			dest := waterutil.IPv4Destination(pack).To4()
			mkey, _ := binary.Uvarint(dest)

			log.Debugf("ip dest: %v", dest)
			if hpeer, found := srv.peers.peersByIP[mkey]; found {
				srv.SendToClient(hpeer, &DataPacket{Payload:pack})
			} else {
				log.Warningf("client peer with key %d not found", mkey)
			}

		case packet := <-srv.fromNet:
			hPack, remainBytes, err := unpackHopPacket(packet.data)
			if err != nil {
				log.Error(err.Error())
			} else {
				if handle_func, ok := srv.pktHandle[hPack.Proto]; ok {
					peer, ok := srv.peers.PeersByID[hPack.Sid]
					if !ok {
						if hPack.Proto == HOP_FLG_HSH {
							_, err := srv.peers.NewPeer(hPack.Sid)
							if err != nil {
								log.Errorf("Cant alloc IP from pool %v", err)
							}

							peer, _ = srv.peers.PeersByID[hPack.Sid]
						}

					}

					if peer != nil {
						peer.lastSeenTime = time.Now()
						if handle_func != nil {
							handle_func(peer, hPack)
						}
					}

				} else {
					log.Errorf("Unkown flag: %x", hPack.Proto)
				}
			}
		}
	}
}

func (srv *CandyVPNServer) SendToClient(peer *VPNPeer, p *AppPacket) {
	hp := NewHopPacket(peer.NextSeq(), p)
	log.Debugf("peer: %v", peer)
	upacket := &RawPacket{data:hp.Pack(), conn:peer.RandomConn()}
	srv.toNet <- upacket
}

func (srv *CandyVPNServer) handlePing(hpeer *VPNPeer, hp *HopPacket) {
	if hpeer.state == HOP_STAT_WORKING {
		srv.SendToClient(hpeer, new(PingAckPacket))
	}
}

func (srv *CandyVPNServer) handleHandshake(peer *VPNPeer, hp *HopPacket) {
	log.Debugf("assign address %s", peer.ip)
	atomic.StoreInt32(&peer.state, HOP_STAT_HANDSHAKE)

	srv.SendToClient(peer,
		HandshakeAckPacket{
			Ip:peer.ip,
			MaskSize:srv.peers.ippool.subnet.Mask.Size()[0]},
	)
	go func() {
		select {
		case <-peer.hsDone:
			peer.state = HOP_STAT_WORKING
			return
		case <-time.After(6 * time.Second):
			srv.SendToClient(peer, new(FinPacket))
			srv.peers.DeletePeer(peer.sid)
		}
	}()
}

func (srv *CandyVPNServer) handleHandshakeAck(peer *VPNPeer, hp *HopPacket) {
	log.Infof("Client %d Connected", peer.ip)
	if ok := atomic.CompareAndSwapInt32(&peer.state, HOP_STAT_HANDSHAKE, HOP_STAT_WORKING); ok {
		peer.hsDone <- struct{}{}
	} else {
		log.Warningf("Invalid peer state: %v", peer.ip)
		srv.peers.DeletePeer(peer.sid)
		srv.SendToClient(peer, new(FinPacket))
	}
}

func (srv *CandyVPNServer) handleDataPacket(peer *VPNPeer, hp *HopPacket) {
	if peer.state == HOP_STAT_WORKING {
		srv.toIface <- hp
	}
}

func (srv *CandyVPNServer) handleFinish(peer *VPNPeer, hp *HopPacket) {
	log.Infof("Releasing client sid: %d", peer.sid)
	srv.peers.DeletePeer(peer.sid)
	srv.SendToClient(peer, new(FinAckPacket))
}

func (srv *CandyVPNServer) cleanUp() {

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)
	<-c

	for _, peer := range srv.peers.PeersByID {
		srv.SendToClient(peer, new(FinAckPacket))
	}
	os.Exit(0)
}

func (srv *CandyVPNServer) peerTimeoutWatcher() {
	timeout := time.Second * time.Duration(srv.cfg.PeerTimeout)

	for {
		select {
		case <-time.After(timeout):
			for sid, peer := range srv.peers.PeersByID {
				log.Debugf("IP: %v, sid: %v", peer.ip, sid)
				srv.SendToClient(peer, new(PingPacket))
			}
		case peer := <-srv.peers.PeerTimeout:
			srv.SendToClient(peer, new(FinPacket))
		}
	}
}

