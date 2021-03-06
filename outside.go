package nebula

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/flynn/noise"
	"github.com/golang/protobuf/proto"
	"github.com/slackhq/nebula/cert"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
)

const (
	minFwPacketLen = 4
)

func (f *Interface) readOutsidePackets(addr *udpAddr, out []byte, packet []byte, header *Header, fwPacket *FirewallPacket, lhh *LightHouseHandler, nb []byte) {
	err := header.Parse(packet)
	if err != nil {
		// TODO: best if we return this and let caller log
		// TODO: Might be better to send the literal []byte("holepunch") packet and ignore that?
		// Hole punch packets are 0 or 1 byte big, so lets ignore printing those errors
		if len(packet) > 1 {
			l.Info(
				"error parsing inbound packet",
				zap.Any("packet", packet),
				zap.Error(err),
				zap.Any("from", addr),
			)
		}
		return
	}

	//l.Error("in packet ", header, packet[HeaderLen:])

	// verify if we've seen this index before, otherwise respond to the handshake initiation
	hostinfo, err := f.hostMap.QueryIndex(header.RemoteIndex)

	var ci *ConnectionState
	if err == nil {
		ci = hostinfo.ConnectionState
	}

	switch header.Type {
	case message:
		if !f.handleEncrypted(ci, addr, header) {
			return
		}

		f.decryptToTun(hostinfo, header.MessageCounter, out, packet, fwPacket, nb)

		// Fallthrough to the bottom to record incoming traffic

	case lightHouse:
		f.messageMetrics.Rx(header.Type, header.Subtype, 1)
		if !f.handleEncrypted(ci, addr, header) {
			return
		}

		d, err := f.decrypt(hostinfo, header.MessageCounter, out, packet, header, nb)
		if err != nil {
			hostinfo.logger().Error(
				"failed to decrypt lighthouse packet",
				zap.Any("packet", packet),
				zap.Uint32("udpIp", addr.IP),
				zap.Uint16("udpPort", addr.Port),
			)

			//TODO: maybe after build 64 is out? 06/14/2018 - NB
			//f.sendRecvError(net.Addr(addr), header.RemoteIndex)
			return
		}
		lhh.HandleRequest(addr, hostinfo.hostId, d, hostinfo.GetCert(), f)

		// Fallthrough to the bottom to record incoming traffic

	case test:
		f.messageMetrics.Rx(header.Type, header.Subtype, 1)
		if !f.handleEncrypted(ci, addr, header) {
			return
		}

		d, err := f.decrypt(hostinfo, header.MessageCounter, out, packet, header, nb)
		if err != nil {
			hostinfo.logger().Error(
				"failed to decrypt test packet",
				zap.Any("packet", packet),
				zap.Uint32("udpIp", addr.IP),
				zap.Uint16("udpPort", addr.Port),
			)

			//TODO: maybe after build 64 is out? 06/14/2018 - NB
			//f.sendRecvError(net.Addr(addr), header.RemoteIndex)
			return
		}

		if header.Subtype == testRequest {
			// This testRequest might be from TryPromoteBest, so we should roam
			// to the new IP address before responding
			f.handleHostRoaming(hostinfo, addr)
			f.send(test, testReply, ci, hostinfo, hostinfo.remote, d, nb, out)
		}

		// Fallthrough to the bottom to record incoming traffic

		// Non encrypted messages below here, they should not fall through to avoid tracking incoming traffic since they
		// are unauthenticated

	case handshake:
		f.messageMetrics.Rx(header.Type, header.Subtype, 1)
		HandleIncomingHandshake(f, addr, packet, header, hostinfo)
		return

	case recvError:
		f.messageMetrics.Rx(header.Type, header.Subtype, 1)
		// TODO: Remove this with recv_error deprecation
		f.handleRecvError(addr, header)
		return

	case closeTunnel:
		f.messageMetrics.Rx(header.Type, header.Subtype, 1)
		if !f.handleEncrypted(ci, addr, header) {
			return
		}
		hostinfo.logger().Info(
			"close tunnel received, tearing down",
			zap.Uint32("udpIp", addr.IP),
			zap.Uint16("udpPort", addr.Port),
		)
		f.closeTunnel(hostinfo)
		return

	default:
		f.messageMetrics.Rx(header.Type, header.Subtype, 1)
		hostinfo.logger().Sugar().Debugf("Unexpected packet received from %s", addr)
		return
	}

	f.handleHostRoaming(hostinfo, addr)

	f.connectionManager.In(hostinfo.hostId)
}

func (f *Interface) closeTunnel(hostInfo *HostInfo) {
	//TODO: this would be better as a single function in ConnectionManager that handled locks appropriately
	f.connectionManager.ClearIP(hostInfo.hostId)
	f.connectionManager.ClearPendingDeletion(hostInfo.hostId)
	f.lightHouse.DeleteVpnIP(hostInfo.hostId)
	f.hostMap.DeleteHostInfo(hostInfo)
}

func (f *Interface) handleHostRoaming(hostinfo *HostInfo, addr *udpAddr) {
	if hostDidRoam(hostinfo.remote, addr) {
		if !f.lightHouse.remoteAllowList.Allow(udp2ipInt(addr)) {
			hostinfo.logger().Debug("lighthouse.remote_allow_list denied roaming", zap.Any("newAddr", addr))
			return
		}
		if !hostinfo.lastRoam.IsZero() && addr.Equals(hostinfo.lastRoamRemote) && time.Since(hostinfo.lastRoam) < RoamingSupressSeconds*time.Second {
			l.Debug(
				"suppressing roam back to previous remote",
				zap.Any("supressDurationSecs", RoamingSupressSeconds),
				zap.Uint32("udpIp", hostinfo.remote.IP),
				zap.Uint16("udpPort", hostinfo.remote.Port),
			)
			return
		}
		l.Info(
			"host roamed to new udp ip/port",
			zap.Uint32("udpIp", hostinfo.remote.IP),
			zap.Uint16("udpPort", hostinfo.remote.Port),
			zap.Uint32("newAddrIp", addr.IP),
			zap.Uint16("newAddrPort", addr.Port),
		)

		hostinfo.lastRoam = time.Now()
		remoteCopy := *hostinfo.remote
		hostinfo.lastRoamRemote = &remoteCopy
		hostinfo.SetRemote(*addr)
		if f.lightHouse.amLighthouse {
			f.lightHouse.AddRemote(hostinfo.hostId, addr, false)
		}
	}

}

func (f *Interface) handleEncrypted(ci *ConnectionState, addr *udpAddr, header *Header) bool {
	// If connectionstate exists and the replay protector allows, process packet
	// Else, send recv errors for 300 seconds after a restart to allow fast reconnection.
	if ci == nil || !ci.window.Check(header.MessageCounter) {
		f.sendRecvError(addr, header.RemoteIndex)
		return false
	}

	return true
}

// newPacket validates and parses the interesting bits for the firewall out of the ip and sub protocol headers
func newPacket(data []byte, incoming bool, fp *FirewallPacket) error {
	// Do we at least have an ipv4 header worth of data?
	if len(data) < ipv4.HeaderLen {
		return fmt.Errorf("packet is less than %v bytes", ipv4.HeaderLen)
	}

	// Is it an ipv4 packet?
	if int((data[0]>>4)&0x0f) != 4 {
		return fmt.Errorf("packet is not ipv4, type: %v", int((data[0]>>4)&0x0f))
	}

	// Adjust our start position based on the advertised ip header length
	ihl := int(data[0]&0x0f) << 2

	// Well formed ip header length?
	if ihl < ipv4.HeaderLen {
		return fmt.Errorf("packet had an invalid header length: %v", ihl)
	}

	// Check if this is the second or further fragment of a fragmented packet.
	flagsfrags := binary.BigEndian.Uint16(data[6:8])
	fp.Fragment = (flagsfrags & 0x1FFF) != 0

	// Firewall handles protocol checks
	fp.Protocol = data[9]

	// Accounting for a variable header length, do we have enough data for our src/dst tuples?
	minLen := ihl
	if !fp.Fragment && fp.Protocol != fwProtoICMP {
		minLen += minFwPacketLen
	}
	if len(data) < minLen {
		return fmt.Errorf("packet is less than %v bytes, ip header len: %v", minLen, ihl)
	}

	// Firewall packets are locally oriented
	if incoming {
		fp.RemoteIP = binary.BigEndian.Uint32(data[12:16])
		fp.LocalIP = binary.BigEndian.Uint32(data[16:20])
		if fp.Fragment || fp.Protocol == fwProtoICMP {
			fp.RemotePort = 0
			fp.LocalPort = 0
		} else {
			fp.RemotePort = binary.BigEndian.Uint16(data[ihl : ihl+2])
			fp.LocalPort = binary.BigEndian.Uint16(data[ihl+2 : ihl+4])
		}
	} else {
		fp.LocalIP = binary.BigEndian.Uint32(data[12:16])
		fp.RemoteIP = binary.BigEndian.Uint32(data[16:20])
		if fp.Fragment || fp.Protocol == fwProtoICMP {
			fp.RemotePort = 0
			fp.LocalPort = 0
		} else {
			fp.LocalPort = binary.BigEndian.Uint16(data[ihl : ihl+2])
			fp.RemotePort = binary.BigEndian.Uint16(data[ihl+2 : ihl+4])
		}
	}

	return nil
}

func (f *Interface) decrypt(hostinfo *HostInfo, mc uint64, out []byte, packet []byte, header *Header, nb []byte) ([]byte, error) {
	var err error
	out, err = hostinfo.ConnectionState.dKey.DecryptDanger(out, packet[:HeaderLen], packet[HeaderLen:], mc, nb)
	if err != nil {
		return nil, err
	}

	if !hostinfo.ConnectionState.window.Update(mc) {
		hostinfo.logger().Debug(
			"dropping out of window packet",
			zap.Any("header", header),
		)
		return nil, errors.New("out of window packet")
	}

	return out, nil
}

func (f *Interface) decryptToTun(hostinfo *HostInfo, messageCounter uint64, out []byte, packet []byte, fwPacket *FirewallPacket, nb []byte) {
	var err error

	out, err = hostinfo.ConnectionState.dKey.DecryptDanger(out, packet[:HeaderLen], packet[HeaderLen:], messageCounter, nb)
	if err != nil {
		hostinfo.logger().Error("Failed to decrypt packet", zap.Error(err))
		//TODO: maybe after build 64 is out? 06/14/2018 - NB
		//f.sendRecvError(hostinfo.remote, header.RemoteIndex)
		return
	}

	err = newPacket(out, true, fwPacket)
	if err != nil {
		hostinfo.logger().Error(
			"failed to validate inbound packet",
			zap.Error(err),
			zap.Any("packet", out),
		)
		return
	}

	if !hostinfo.ConnectionState.window.Update(messageCounter) {
		hostinfo.logger().Debug(
			"dropping out of window packet",
			zap.Any("fwPacket", fwPacket),
		)
		return
	}

	dropReason := f.firewall.Drop(out, *fwPacket, true, hostinfo, trustedCAs)
	if dropReason != nil {
		hostinfo.logger().Debug(
			"dropping inbound packet",
			zap.String("reason", dropReason.Error()),
			zap.Any("fwPacket", fwPacket),
		)
		return
	}

	f.connectionManager.In(hostinfo.hostId)
	err = f.inside.WriteRaw(out)
	if err != nil {
		l.Error("Failed to write to tun", zap.Error(err))
	}
}

func (f *Interface) sendRecvError(endpoint *udpAddr, index uint32) {
	f.messageMetrics.Tx(recvError, 0, 1)

	//TODO: this should be a signed message so we can trust that we should drop the index
	b := HeaderEncode(make([]byte, HeaderLen), Version, uint8(recvError), 0, index, 0)
	f.outside.WriteTo(b, endpoint)
	l.Debug(
		"recv error sent",
		zap.Uint32("index", index),
		zap.Uint32("udpIp", endpoint.IP),
		zap.Uint16("udpPort", endpoint.Port),
	)
}

func (f *Interface) handleRecvError(addr *udpAddr, h *Header) {
	// This flag is to stop caring about recv_error from old versions
	// This should go away when the old version is gone from prod
	l.Debug(
		"recv error received",
		zap.Uint32("index", h.RemoteIndex),
		zap.Uint32("udpIp", addr.IP),
		zap.Uint16("udpPort", addr.Port),
	)
	hostinfo, err := f.hostMap.QueryReverseIndex(h.RemoteIndex)
	if err != nil {
		l.Debug(err.Error(), zap.Uint32("index", h.RemoteIndex))
		return
	}

	if !hostinfo.RecvErrorExceeded() {
		return
	}
	if hostinfo.remote != nil && hostinfo.remote.String() != addr.String() {
		l.Warn(
			"someone spoofing recv_errors??",
			zap.Uint32("udpIp", addr.IP),
			zap.Uint16("udpPort", addr.Port),
			zap.Uint32("remoteIp", addr.IP),
			zap.Uint16("remotePort", addr.Port),
		)
		return
	}

	// We delete this host from the main hostmap
	f.hostMap.DeleteHostInfo(hostinfo)
	// We also delete it from pending to allow for
	// fast reconnect. We must null the connectionstate
	// or a counter reuse may happen
	hostinfo.ConnectionState = nil
	f.handshakeManager.DeleteHostInfo(hostinfo)
}

/*
func (f *Interface) sendMeta(ci *ConnectionState, endpoint *net.UDPAddr, meta *NebulaMeta) {
	if ci.eKey != nil {
		//TODO: log error?
		return
	}

	msg, err := proto.Marshal(meta)
	if err != nil {
		l.Debugln("failed to encode header")
	}

	c := ci.messageCounter
	b := HeaderEncode(nil, Version, uint8(metadata), 0, hostinfo.remoteIndexId, c)
	ci.messageCounter++

	msg := ci.eKey.EncryptDanger(b, nil, msg, c)
	//msg := ci.eKey.EncryptDanger(b, nil, []byte(fmt.Sprintf("%d", counter)), c)
	f.outside.WriteTo(msg, endpoint)
}
*/

func RecombineCertAndValidate(h *noise.HandshakeState, rawCertBytes []byte) (*cert.NebulaCertificate, error) {
	pk := h.PeerStatic()

	if pk == nil {
		return nil, errors.New("no peer static key was present")
	}

	if rawCertBytes == nil {
		return nil, errors.New("provided payload was empty")
	}

	r := &cert.RawNebulaCertificate{}
	err := proto.Unmarshal(rawCertBytes, r)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling cert: %s", err)
	}

	// If the Details are nil, just exit to avoid crashing
	if r.Details == nil {
		return nil, fmt.Errorf("certificate did not contain any details")
	}

	r.Details.PublicKey = pk
	recombined, err := proto.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("error while recombining certificate: %s", err)
	}

	c, _ := cert.UnmarshalNebulaCertificate(recombined)
	isValid, err := c.Verify(time.Now(), trustedCAs)
	if err != nil {
		return c, fmt.Errorf("certificate validation failed: %s", err)
	} else if !isValid {
		// This case should never happen but here's to defensive programming!
		return c, errors.New("certificate validation failed but did not return an error")
	}

	return c, nil
}
