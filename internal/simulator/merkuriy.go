package simulator

import (
"fmt"
"net"
)

func RunMerkuriy(addr string) error {
ln, err := net.Listen("tcp", addr)
if err != nil {
return err
}
defer ln.Close()

for {
conn, err := ln.Accept()
if err != nil {
return err
}
go func(c net.Conn) {
defer c.Close()
buf := make([]byte, 512)
for {
n, err := c.Read(buf)
if err != nil {
return
}
if n == 0 {
continue
}
_, _ = c.Write([]byte{0x01, 0x00})
}
}(conn)
}
}

func Run(device string, addr string) error {
switch device {
case "merkuriy":
return RunMerkuriy(addr)
case "vkm360":
return RunVKM360(addr)
case "ivk-ter":
return RunIVKTER(addr)
case "tsrv024":
return RunTSRV024(addr)
default:
return fmt.Errorf("unsupported simulator: %s", device)
}
}