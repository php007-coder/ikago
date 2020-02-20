package pcap

import (
	"errors"
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"net"
)

// Pcap describes a packet capture
type Pcap struct {
	LocalPort    uint16
	RemoteIP     net.IP
	RemotePort   uint16
	localDevs    []Device
	RemoteDev    *Device
	IsLocalOnly  bool
	gatewayDev   *Device
	localHandles []*pcap.Handle
	remoteHandle *pcap.Handle
	remoteSeq    uint32
	remoteId     uint16
	localSeq     uint32
	localId      uint16
}

// Open implements a method opens the pcap
func (p *Pcap) Open() error {
	devs, err := FindAllDevs()
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	p.localDevs = make([]Device, 0)
	for _, dev := range devs {
		if p.IsLocalOnly {
			if dev.IsLoop {
				p.localDevs = append(p.localDevs, dev)
			}
		} else {
			p.localDevs = append(p.localDevs, dev)
		}
	}

	if p.RemoteDev == nil {
		gatewayAddr, err := FindGatewayAddr()
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		devs, err := FindAllDevs()
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		for _, dev := range devs {
			if dev.IsLoop {
				continue
			}
			for _, addr := range dev.IPAddrs {
				ipnet := net.IPNet{IP:addr.IP, Mask:addr.Mask}
				if ipnet.Contains(gatewayAddr.IP) {
					p.gatewayDev, err = FindGatewayDev(dev.Name)
					if err != nil {
						continue
					}
					p.RemoteDev = &Device{
						Name:         dev.Name,
						FriendlyName: dev.FriendlyName,
						IPAddrs:      append(make([]IPAddr, 0), addr),
						HardwareAddr: dev.HardwareAddr,
						IsLoop:       dev.IsLoop,
					}
					break
				}
			}
			if p.RemoteDev != nil {
				break
			}
		}
	} else {
		if p.RemoteDev.IsLoop {
			p.gatewayDev = p.RemoteDev
		} else {
			var err error
			p.gatewayDev, err = FindGatewayDev(p.RemoteDev.Name)
			if err != nil {
				return fmt.Errorf("open: %w", err)
			}
		}
	}
	if p.localDevs == nil || len(p.localDevs) <= 0 || p.RemoteDev == nil || p.gatewayDev == nil {
		return fmt.Errorf("open: %w", errors.New("can not determine device"))
	}
	strDevs := ""
	for i, dev := range p.localDevs {
		if i != 0 {
			strDevs = strDevs + ", "
		}
		strIPs := ""
		for j, addr := range dev.IPAddrs {
			if j != 0 {
				strIPs = strIPs + fmt.Sprintf(", %s", addr.IP)
			} else {
				strIPs = strIPs + addr.IP.String()
			}
		}
		if dev.IsLoop {
			strDevs = strDevs + fmt.Sprintf("%s: %s", dev.FriendlyName, strIPs)
		} else {
			strDevs = strDevs + fmt.Sprintf("%s [%s]: %s", dev.FriendlyName, dev.HardwareAddr, strIPs)
		}
	}
	fmt.Printf("Listen on %s\n", strDevs)
	if !p.gatewayDev.IsLoop {
		fmt.Printf("Route upstream from %s [%s]: %s to gateway [%s]: %s\n", p.RemoteDev.FriendlyName,
			p.RemoteDev.HardwareAddr, p.remoteDevIP(), p.gatewayDev.HardwareAddr, p.gatewayDevIP())
	} else {
		fmt.Printf("Route upstream to loopback %s\n", p.RemoteDev.FriendlyName)
	}

	p.localHandles = make([]*pcap.Handle, 0)
	for _, dev := range p.localDevs {
		localHandle, err := pcap.OpenLive(dev.Name, 1600, true, pcap.BlockForever)
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		err = localHandle.SetBPFFilter(fmt.Sprintf("tcp && dst port %d", p.LocalPort))
		p.localHandles = append(p.localHandles, localHandle)
	}
	for _, handle := range p.localHandles {
		localPacketSrc := gopacket.NewPacketSource(handle, handle.LinkType())
		go func() {
			for packet := range localPacketSrc.Packets() {
				p.handleLocal(packet)
			}
		}()
	}

	p.remoteHandle, err = pcap.OpenLive(p.RemoteDev.Name, 1600, true, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	err = p.remoteHandle.SetBPFFilter(fmt.Sprintf("tcp && src host %s && src port %d && dst port %d",
		p.RemoteIP, p.RemotePort, p.LocalPort))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	remotePacketSrc := gopacket.NewPacketSource(p.remoteHandle, p.remoteHandle.LinkType())
	go func() {
		for packet := range remotePacketSrc.Packets() {
			p.handleRemote(packet)
		}
	}()

	select {}
}

// Close implements a method closes the pcap
func (p *Pcap) Close() {
	for _, handle := range p.localHandles {
		handle.Close()
	}
	p.remoteHandle.Close()
}

func (p *Pcap) remoteDevIP() net.IP {
	return p.RemoteDev.IPAddrs[0].IP
}

func (p *Pcap) gatewayDevIP() net.IP {
	return p.gatewayDev.IPAddrs[0].IP
}

func (p *Pcap) handleLocal(packet gopacket.Packet) {
	networkLayer := packet.NetworkLayer()
	if networkLayer == nil {
		fmt.Println(fmt.Errorf("handle local: %w", errors.New("missing network layer")))
		return
	}
	networkLayerType := networkLayer.LayerType()
	switch networkLayerType {
	case layers.LayerTypeIPv4, layers.LayerTypeIPv6:
		break
	default:
		fmt.Println(fmt.Errorf("handle local: %w", fmt.Errorf("not support %s", networkLayerType)))
		return
	}
	transportLayer := packet.TransportLayer()
	if transportLayer == nil {
		fmt.Println(fmt.Errorf("handle local: %w", errors.New("missing transport layer")))
		return
	}
	applicationLayer := packet.ApplicationLayer()

	contents := append(make([]byte, 0), networkLayer.LayerContents()...)
	contents = append(make([]byte, 0), transportLayer.LayerContents()...)
	if applicationLayer != nil {
		contents = append(make([]byte, 0), applicationLayer.LayerContents()...)
	}

	newTransportLayer := createTCP(p.LocalPort, p.RemotePort, p.remoteSeq)
	p.remoteSeq++

	isRemoteDevIPv4 := p.remoteDevIP().To4() != nil
	isGatewayDevIPv4 := p.gatewayDevIP().To4() != nil
	var isIPv4 bool
	if isRemoteDevIPv4 && isGatewayDevIPv4 {
		isIPv4 = true
	} else if !isRemoteDevIPv4 && !isGatewayDevIPv4 {
		isIPv4 = false
	} else {
		fmt.Println(fmt.Errorf("handle local: %w", errors.New("not support ipv6 transition")))
		return
	}

	var newNetworkLayer gopacket.NetworkLayer
	if isIPv4 {
		newNetworkLayer = createIPv4(p.remoteDevIP(), p.RemoteIP, p.remoteId, 64)
		p.remoteId++

		ipv4 := newNetworkLayer.(*layers.IPv4)

		newTransportLayer.Checksum = CheckTCPIPv4Sum(newTransportLayer, contents, ipv4)

		ipv4.Length = (uint16(ipv4.IHL) + uint16(len(newTransportLayer.LayerContents())) + uint16(len(contents))) * 8
		ipv4.Checksum = checkSum(ipv4.LayerContents())
	} else {
		fmt.Println(fmt.Errorf("handle local: %w", errors.New("not support ipv6")))
		return
	}

	var newLinkLayer gopacket.Layer
	newNetworkLayerType := newNetworkLayer.LayerType()
	if p.RemoteDev.IsLoop {
		newLinkLayer = &layers.Loopback{}
	} else {
		var t layers.EthernetType
		switch newNetworkLayerType {
		case layers.LayerTypeIPv4:
			t = layers.EthernetTypeIPv4
		default:
			fmt.Println(fmt.Errorf("handle local: %w", fmt.Errorf("not support %s", newNetworkLayerType)))
			return
		}
		newLinkLayer = &layers.Ethernet{
			SrcMAC:       p.RemoteDev.HardwareAddr,
			DstMAC:       p.gatewayDev.HardwareAddr,
			EthernetType: t,
		}
	}

	options := gopacket.SerializeOptions{}
	buffer := gopacket.NewSerializeBuffer()
	var err error
	newLinkLayerType := newLinkLayer.LayerType()
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		switch newNetworkLayerType {
		case layers.LayerTypeIPv4:
			err = gopacket.SerializeLayers(buffer, options,
				newLinkLayer.(*layers.Loopback),
				newNetworkLayer.(*layers.IPv4),
				newTransportLayer,
				gopacket.Payload(contents),
			)
		default:
			fmt.Println(fmt.Errorf("handle local: %w", fmt.Errorf("not support %s", newNetworkLayerType)))
			return
		}
	case layers.LayerTypeEthernet:
		switch newNetworkLayerType {
		case layers.LayerTypeIPv4:
			err = gopacket.SerializeLayers(buffer, options,
				newLinkLayer.(*layers.Ethernet),
				newNetworkLayer.(*layers.IPv4),
				newTransportLayer,
				gopacket.Payload(contents),
			)
		default:
			fmt.Println(fmt.Errorf("handle local: %w", fmt.Errorf("not support %s", newNetworkLayerType)))
			return
		}
	default:
		fmt.Println(fmt.Errorf("handle local: %w", fmt.Errorf("not support %s", newLinkLayerType)))
		return
	}
	if err != nil {
		fmt.Println(fmt.Errorf("handle local: %w", err))
		return
	}

	err = p.remoteHandle.WritePacketData(buffer.Bytes())
	if err != nil {
		fmt.Println(fmt.Errorf("handle local: %w", errors.New("cannot write packet data")))
	}
}

func (p *Pcap) handleRemote(packet gopacket.Packet) {

}

func createTCP(srcPort, dstPort uint16, seq uint32) *layers.TCP {
	return &layers.TCP{
		SrcPort:    layers.TCPPort(srcPort),
		DstPort:    layers.TCPPort(dstPort),
		Seq:        seq,
		DataOffset: 5,
		PSH:        true,
		ACK:        true,
		// Checksum:   0,
	}
}

func createIPv4(srcIP, dstIP net.IP, id uint16, ttl uint8) *layers.IPv4 {
	return &layers.IPv4{
		Version:    4,
		IHL:        5,
		// Length:     0,
		Id:         id,
		Flags:      layers.IPv4DontFragment,
		TTL:        ttl,
		Protocol:   layers.IPProtocolTCP,
		// Checksum:   0,
		SrcIP:      srcIP,
		DstIP:      dstIP,
	}
}
