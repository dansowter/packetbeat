package main

import (
    "flag"
    "fmt"
    "io/ioutil"
    "log"
    "os"
    "runtime"
    "runtime/pprof"
    "strconv"
    "strings"
    "time"

    "github.com/BurntSushi/toml"
    "github.com/akrennmair/gopcap"
    "github.com/nranchev/go-libGeoIP"
)

const Version = "0.3.1"

type Packet struct {
    ts      time.Time
    tuple   IpPortTuple
    payload []byte
}

type protocolType uint16

const (
    UnknownProtocol protocolType = iota
    HttpProtocol
    MysqlProtocol
    RedisProtocol
    PgsqlProtocol
)

var protocolNames = []string{"unknown", "http", "mysql", "redis", "pgsql"}

type tomlConfig struct {
    Interfaces tomlInterfaces
    RunOptions tomlRunOptions
    Protocols  map[string]tomlProtocol
    Procs      tomlProcs
    Output     map[string]tomlMothership
    Agent      tomlAgent
    Logging    tomlLogging
    Passwords  tomlPasswords
}

type tomlInterfaces struct {
    Device string
}

type tomlRunOptions struct {
    Uid int
    Gid int
}

type tomlLogging struct {
    Selectors []string
}

type tomlPasswords struct {
    Hide_keywords []string
}

var _Config tomlConfig
var _ConfigMeta toml.MetaData
var _GeoLite *libgeo.GeoIP

func Bytes_Ipv4_Ntoa(bytes []byte) string {
    var strarr []string = make([]string, 4)
    for i, b := range bytes {
        strarr[i] = strconv.Itoa(int(b))
    }
    return strings.Join(strarr, ".")
}

func decodePktEth(datalink int, pkt *pcap.Packet) {
    defer RECOVER("decodePktEth exception")

    packet := new(Packet)
    var l2hlen int
    var eth_type uint16

    if datalink != pcap.LINKTYPE_LINUX_SLL {
        // we are just assuming ETH
        l2hlen = 14

        // bytes 12 and 13 are the pkt type
        eth_type = Bytes_Ntohs(pkt.Data[12:14])

    } else {
        l2hlen = 16

        // bytes 14 and 15 are the pkt type
        eth_type = Bytes_Ntohs(pkt.Data[14:16])
    }

    if eth_type != 0x800 {
        DEBUG("ip", "Not ipv4 packet. Ignoring")
        return
    }

    if len(pkt.Data) < l2hlen+20 {
        DEBUG("ip", "Packet too short to be ethernet")
        return
    }

    // IP header
    iphl := int((uint16(pkt.Data[l2hlen]) & 0x0f) * 4)
    if len(pkt.Data) < l2hlen+iphl {
        DEBUG("ip", "Packet too short to be IP")
        return
    }
    iphdr := pkt.Data[l2hlen : l2hlen+iphl]

    //DEBUG("Packet timestamp: %s", pkt.Time)
    packet.ts = pkt.Time

    packet.tuple.Src_ip = Bytes_Ntohl(iphdr[12:16])
    packet.tuple.Dst_ip = Bytes_Ntohl(iphdr[16:20])

    mf := (uint8(iphdr[6]&0x20) != 0)
    frag_offset := (uint16(iphdr[7]) << 3) | (uint16(iphdr[6]) & 0x1F)

    if mf || frag_offset > 0 {
        DEBUG("ip", "Fragmented packets not yet supported")
        return
    }

    ip_length := int(Bytes_Ntohs(iphdr[2:4]))

    protocol := uint8(iphdr[9])
    if protocol != 6 {
        DEBUG("ip", "Not TCP packet. Ignoring")
        return
    }

    if len(pkt.Data) < l2hlen+iphl+20 {
        DEBUG("ip", "Packet too short to be TCP")
        return
    }

    tcphl := int((uint16(pkt.Data[l2hlen+iphl+12]) >> 4) * 4)
    if tcphl > 20 && len(pkt.Data) < l2hlen+iphl+tcphl {
        DEBUG("ip", "Packet too short to contain TCP header")
        return
    }

    tcphdr := pkt.Data[l2hlen+iphl : l2hlen+iphl+tcphl]
    packet.tuple.Src_port = Bytes_Ntohs(tcphdr[0:2])
    packet.tuple.Dst_port = Bytes_Ntohs(tcphdr[2:4])

    //DEBUG(" %s:%d -> %s:%d", packet.tuple.src_ip, packet.tuple.src_port,
    //      packet.tuple.dst_ip, packet.tuple.dst_port)

    data_offset := (tcphdr[12] >> 4) * 4

    if l2hlen+iphl+int(data_offset) > l2hlen+ip_length {
        DEBUG("ip", "data_offset pointing outside of packet")
        return
    }

    if len(pkt.Data) < l2hlen+ip_length {
        DEBUG("ip", "Captured packet smaller then advertised in IP layer")
        return
    }

    packet.payload = pkt.Data[l2hlen+iphl+int(data_offset) : l2hlen+ip_length]

    FollowTcp(tcphdr, packet)
}

func writeHeapProfile(filename string) {
    f, err := os.Create(filename)
    if err != nil {
        ERR("Failed creating file %s: %s", filename, err)
        return
    }
    pprof.WriteHeapProfile(f)
    f.Close()

    INFO("Created memory profile file %s.", filename)
}

func debugMemStats() {
    var m runtime.MemStats
    runtime.ReadMemStats(&m)
    DEBUG("mem", "Memory stats: In use: %d Total (even if freed): %d System: %d",
        m.Alloc, m.TotalAlloc, m.Sys)
}

func main() {

    // Use our own FlagSet, because some libraries pollute the global one
    var cmdLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

    configfile := cmdLine.String("c", "packetbeat.conf", "Configuration file")
    file := cmdLine.String("I", "", "file")
    loop := cmdLine.Int("l", 1, "Loop file. 0 - loop forever")
    debugSelectorsStr := cmdLine.String("d", "", "Enable certain debug selectors")
    oneAtAtime := cmdLine.Bool("O", false, "Read packets one at a time (press Enter)")
    toStdout := cmdLine.Bool("e", false, "Output to stdout instead of syslog")
    topSpeed := cmdLine.Bool("t", false, "Read packets as fast as possible, without sleeping")
    publishDisabled := cmdLine.Bool("N", false, "Disable actual publishing for testing")
    verbose := cmdLine.Bool("v", false, "Log at INFO level")
    printVersion := cmdLine.Bool("version", false, "Print version and exit")
    memprofile := cmdLine.String("memprofile", "", "write memory profile to this file")

    cmdLine.Parse(os.Args[1:])

    if *printVersion {
        fmt.Printf("Packetbeat version %s (%s)\n", Version, runtime.GOARCH)
        return
    }

    logLevel := LOG_ERR
    if *verbose {
        logLevel = LOG_INFO
    }

    debugSelectors := []string{}
    if len(*debugSelectorsStr) > 0 {
        debugSelectors = strings.Split(*debugSelectorsStr, ",")
        logLevel = LOG_DEBUG
    }

    var h *pcap.Pcap
    var err error

    if _ConfigMeta, err = toml.DecodeFile(*configfile, &_Config); err != nil {
        fmt.Printf("TOML config parsing failed on %s: %s. Exiting.\n", *configfile, err)
        return
    }

    if len(debugSelectors) == 0 {
        debugSelectors = _Config.Logging.Selectors
    }
    LogInit(logLevel, "", !*toStdout, debugSelectors)

    if !IS_DEBUG("stdlog") {
        // disable standard logging by default
        log.SetOutput(ioutil.Discard)
    }

    if *file != "" {
        h, err = pcap.Openoffline(*file)
        if h == nil {
            ERR("Openoffline(%s) failed: %s", *file, err)
            return
        }
    } else {
        device := _Config.Interfaces.Device

        h, err = pcap.Openlive(device, 65535, true, 0)
        if h == nil {
            ERR("pcap.Openlive failed(%s): %s", device, err)
            return
        }
    }
    defer func() {
        h.Close()
    }()

    if err = DropPrivileges(); err != nil {
        CRIT(err.Error())
        return
    }

    if *file == "" {
        filter := configToFilter(&_Config)
        if filter != "" {
            DEBUG("pcapfilter", "Installing filter '%s'", filter)
            err := h.Setfilter(filter)
            if err != nil {
                ERR("pcap.Setfilter failed: %s", err)
                return
            }
        }
    }

    if err = Publisher.Init(*publishDisabled); err != nil {
        CRIT(err.Error())
        return
    }

    if err = procWatcher.Init(&_Config.Procs); err != nil {
        CRIT(err.Error())
        return
    }

    if err = TcpInit(); err != nil {
        CRIT(err.Error())
        return
    }

    datalink := h.Datalink()
    if datalink != pcap.LINKTYPE_ETHERNET && datalink != pcap.LINKTYPE_LINUX_SLL {
        WARN("Unsupported link type: %d", datalink)
    }

    _GeoLite, err = libgeo.Load("/usr/share/GeoIP/GeoIP.dat")
    if err != nil {
        WARN("Could not load GeoIP data: %s", err.Error())
    }

    counter := 0
    live := true
    loopCount := 1
    var lastPktTime *time.Time = nil
    for live {
        var pkt *pcap.Packet
        var res int32

        if *oneAtAtime {
            fmt.Println("Press enter to read packet")
            fmt.Scanln()
        }

        pkt, res = h.NextEx()
        switch res {
        case -1:
            ERR("pcap.NextEx() error: %s", h.Geterror())
            live = false
            continue
        case -2:
            DEBUG("pcapread", "End of file")
            loopCount += 1
            if *loop > 0 && loopCount > *loop {
                // give a bit of time to the publish goroutine
                // to flush
                time.Sleep(300 * time.Millisecond)
                live = false
                continue
            }

            DEBUG("pcapread", "Reopening the file")
            h.Close()
            h, err = pcap.Openoffline(*file)
            if h == nil {
                ERR("Openoffline(%s) failed: %s", *file, err)
                return
            }
            lastPktTime = nil
            continue
        case 0:
            // timeout
            continue
        }
        if res != 1 {
            panic(fmt.Sprintf("Unexpected return code from pcap.NextEx: %d", res))
        }

        if pkt == nil {
            panic("Nil packet despite res=1")
        }

        if *file != "" {
            if lastPktTime != nil && !*topSpeed {
                sleep := pkt.Time.Sub(*lastPktTime)
                if sleep > 0 {
                    time.Sleep(sleep)
                } else {
                    WARN("Time in pcap went backwards: %d", sleep)
                }
            }
            _lastPktTime := pkt.Time
            lastPktTime = &_lastPktTime
            pkt.Time = time.Now() // overwrite what we get from the pcap
        }
        counter++

        decodePktEth(datalink, pkt)
    }
    INFO("Input finish. Processed %d packets. Have a nice day!", counter)

    if *memprofile != "" {
        // wait for all TCP streams to expire
        time.Sleep(TCP_STREAM_EXPIRY * 1.2)
        PrintTcpMap()
        runtime.GC()

        writeHeapProfile(*memprofile)

        debugMemStats()
    }
}
