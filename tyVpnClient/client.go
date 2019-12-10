package tyVpnClient

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/tachyon-protocol/udw/tyTls"
	"github.com/tachyon-protocol/udw/tyVpnProtocol"
	"github.com/tachyon-protocol/udw/tyVpnRouteServer/tyVpnRouteClient"
	"github.com/tachyon-protocol/udw/udwBinary"
	"github.com/tachyon-protocol/udw/udwBytes"
	"github.com/tachyon-protocol/udw/udwConsole"
	"github.com/tachyon-protocol/udw/udwErr"
	"github.com/tachyon-protocol/udw/udwIo"
	"github.com/tachyon-protocol/udw/udwIpPacket"
	"github.com/tachyon-protocol/udw/udwLog"
	"github.com/tachyon-protocol/udw/udwNet"
	"github.com/tachyon-protocol/udw/udwNet/udwIPNet"
	"github.com/tachyon-protocol/udw/udwNet/udwTapTun"
	"github.com/tachyon-protocol/udw/udwRand"
	"io"
	"net"
	"sync"
	"time"
)

type RunReq struct {
	ServerIp   string
	ServerTKey string

	IsRelay            bool
	ExitServerClientId uint64 //required when IsRelay is true
	ExitServerTKey     string //required when IsRelay is true

	ServerChk                   string // if it is "", it will use InsecureSkipVerify
	DisableUsePublicRouteServer bool
}

type Client struct {
	req                  RunReq
	clientId             uint64
	clientIdToExitServer uint64
	keepAliveChan        chan uint64
	connLock             sync.Mutex
	directVpnConn        net.Conn
	vpnConn              net.Conn
	tlsConfig            *tls.Config
}

func (c *Client) Run(req RunReq) {
	c.req = req
	tyTls.EnableTlsVersion13()
	c.clientId = tyVpnProtocol.GetClientId(0)
	c.clientIdToExitServer = c.clientId
	if c.req.IsRelay {
		c.clientIdToExitServer = tyVpnProtocol.GetClientId(1)
		if c.req.ExitServerClientId == 0 {
			panic("ExitServerClientId can be empty when use relay mode")
		}
	}
	c.tryUseRouteServer()
	tun, err := createTun(c.req.ServerIp)
	udwErr.PanicIfError(err)
	//err = c.connect()
	if c.req.ServerChk == "" {
		c.tlsConfig = newInsecureClientTlsConfig()
	} else {
		var errMsg string
		c.tlsConfig, errMsg = tyTls.NewClientTlsConfigWithChk(tyTls.NewClientTlsConfigWithChkReq{
			ServerChk: c.req.ServerChk,
		})
		udwErr.PanicIfErrorMsg(errMsg)
	}
	c.reconnect()
	c.keepAliveThread()
	go func() {
		vpnPacket := &tyVpnProtocol.VpnPacket{
			Cmd:              tyVpnProtocol.CmdData,
			ClientIdSender:   c.clientIdToExitServer,
			ClientIdReceiver: c.req.ExitServerClientId,
		}
		buf := make([]byte, 16*1024)
		bufW := udwBytes.NewBufWriter(nil)
		c.connLock.Lock()
		vpnConn := c.vpnConn
		c.connLock.Unlock()
		for {
			n, err := tun.Read(buf)
			if err != nil {
				panic("[upe1hcb1q39h] " + err.Error())
			}
			vpnPacket.Data = buf[:n]
			bufW.Reset()
			vpnPacket.Encode(bufW)
			for {
				err = udwBinary.WriteByteSliceWithUint32LenNoAllocV2(vpnConn, bufW.GetBytes())
				if err != nil {
					c.connLock.Lock()
					_vpnConn := c.vpnConn
					c.connLock.Unlock()
					if vpnConn == _vpnConn {
						time.Sleep(time.Millisecond * 50)
					} else {
						vpnConn = _vpnConn
						udwLog.Log("[mpy2nwx1qck] tun read use new vpn conn")
					}
					continue
				}
				break
			}
		}
	}()
	go func() {
		vpnPacket := &tyVpnProtocol.VpnPacket{}
		buf := udwBytes.NewBufWriter(nil)
		c.connLock.Lock()
		vpnConn := c.vpnConn
		c.connLock.Unlock()
		for {
			buf.Reset()
			for {
				err := udwBinary.ReadByteSliceWithUint32LenToBufW(vpnConn, buf)
				if err != nil {
					c.connLock.Lock()
					_vpnConn := c.vpnConn
					c.connLock.Unlock()
					if vpnConn == _vpnConn {
						time.Sleep(time.Millisecond * 50)
					} else {
						vpnConn = _vpnConn
						udwLog.Log("[zdb1mbq1v1kxh] vpn conn read use new vpn conn")
					}
					continue
				}
				break
			}
			err = vpnPacket.Decode(buf.GetBytes())
			udwErr.PanicIfError(err)
			switch vpnPacket.Cmd {
			case tyVpnProtocol.CmdData:
				ipPacket, errMsg := udwIpPacket.NewIpv4PacketFromBuf(vpnPacket.Data)
				if errMsg != "" {
					udwLog.Log("[zdy1mx9y3h]", errMsg)
					continue
				}
				_, err = tun.Write(ipPacket.SerializeToBuf())
				if err != nil {
					udwLog.Log("[wmw12fyr9e] TUN Write error", err)
				}
			case tyVpnProtocol.CmdKeepAlive:
				i := binary.LittleEndian.Uint64(vpnPacket.Data)
				c.keepAliveChan <- i
			default:
				udwLog.Log("[h67hrf4kda] unexpect cmd", vpnPacket.Cmd)
			}
		}
	}()
	udwConsole.WaitForExit()
}

func createTun(vpnServerIp string) (tun io.ReadWriteCloser, err error) {
	vpnClientIp := net.ParseIP("172.21.0.1")
	includeIpNetSet := udwIPNet.NewAllPassIpv4Net()
	includeIpNetSet.RemoveIpString(vpnServerIp)
	tunCreateCtx := &udwTapTun.CreateIpv4TunContext{
		SrcIp:        vpnClientIp,
		DstIp:        vpnClientIp,
		FirstIp:      vpnClientIp,
		DhcpServerIp: vpnClientIp,
		Mtu:          tyVpnProtocol.Mtu,
		Mask:         net.CIDRMask(30, 32),
	}
	err = udwTapTun.CreateIpv4Tun(tunCreateCtx)
	if err != nil {
		return nil, errors.New("[3xa38g7vtd] " + err.Error())
	}
	tunNamed := tunCreateCtx.ReturnTun
	vpnGatewayIp := vpnClientIp
	err = udwErr.PanicToError(func() {
		configLocalNetwork()
		ctx := udwNet.NewRouteContext()
		for _, ipNet := range includeIpNetSet.GetIpv4NetList() {
			goIpNet := ipNet.ToGoIPNet()
			ctx.MustRouteSet(*goIpNet, vpnGatewayIp)
		}
	})
	if err != nil {
		_ = tunNamed.Close()
		return nil, errors.New("[r8y8d5ash4] " + err.Error())
	}
	var closeOnce sync.Once
	return udwIo.StructWriterReaderCloser{
		Reader: tunNamed,
		Writer: tunNamed,
		Closer: udwIo.CloserFunc(func() error {
			closeOnce.Do(func() {
				_ = tunNamed.Close()
				err := udwErr.PanicToError(func() {
					recoverLocalNetwork()
				})
				if err != nil {
					udwLog.Log("error", "uninstallAllPassRoute", err.Error())
				}
			})
			return nil
		}),
	}, nil
}

func newInsecureClientTlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         udwRand.MustCryptoRandToReadableAlpha(5) + ".com",
		InsecureSkipVerify: true,
		NextProtos:         []string{"http/1.1", "h2"},
		MinVersion:         tls.VersionTLS12,
	}
}

func (c *Client) tryUseRouteServer() {
	if c.req.ServerIp == "" {
		if c.req.DisableUsePublicRouteServer {
			panic("need config ServerIp")
		} else {
			routeC := tyVpnRouteClient.Rpc_NewClient(tyVpnProtocol.PublicRouteServerAddr)
			list, rpcErr := routeC.VpnNodeList()
			if rpcErr != nil {
				panic(rpcErr.Error())
			}
			locker := sync.Mutex{}
			var fastNode tyVpnRouteClient.VpnNode
			wg := sync.WaitGroup{}
			for _, node := range list {
				node := node
				wg.Add(1)
				go func() {
					err := Ping(PingReq{
						Ip:        node.Ip,
						ServerChk: node.ServerChk,
					})
					if err == nil {
						locker.Lock()
						if fastNode.Ip == "" {
							fastNode = node
						}
						locker.Unlock()
					}
					wg.Done()
				}()
			}
			wg.Wait()
			if fastNode.Ip == "" {
				panic("all ping lost")
			}
			c.req.ServerIp = fastNode.Ip
			c.req.ServerChk = fastNode.ServerChk
			fmt.Println("ping to get ip [" + c.req.ServerIp + "]")
		}
	}
}
