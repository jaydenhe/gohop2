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

package vpn

import (
	"github.com/FTwOoO/link"
	"github.com/FTwOoO/vpncore/conn"
	"github.com/FTwOoO/vpncore/enc"
)

func CreateServer(tranProtocol conn.TransProtocol, address string, cipher enc.Cipher, pass string, codecProtocol link.Protocol, sessionSendChanSize int) (*link.Server, error) {
	listener, err := conn.NewListener(tranProtocol, address, &enc.BlockConfig{Cipher:cipher, Password:pass})
	if err != nil {
		return nil, err
	}

	return link.NewServer(listener, codecProtocol, sessionSendChanSize), nil
}



func Connect(tranProtocol conn.TransProtocol, address string, cipher enc.Cipher, pass string, codecProtocol link.Protocol, sessionSendChanSize int) (*link.Session, error) {
	c, err := conn.Dial(tranProtocol, address, &enc.BlockConfig{Cipher:cipher, Password:pass})
	if err != nil {
		return nil, err
	}
	codec, _, err := codecProtocol.NewCodec(c)
	if err != nil {
		return nil, err
	}
	return link.NewSession(codec, sessionSendChanSize),  nil
}