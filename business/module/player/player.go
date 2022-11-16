package player

import (
	"github.com/phuhao00/greatestworks-proto/gen/messageId"
	"greatestworks/business/module/chat"
	"greatestworks/business/module/friend"

	"github.com/phuhao00/network"
)

type Player struct {
	UId            uint64
	HandlerParamCh chan *network.Message
	Session        *network.TcpConnX
	FriendSystem   *friend.System
	PrivateChat    *chat.PrivateChat
}

func NewPlayer() *Player {
	p := &Player{
		UId: 0,
	}
	return p
}

func (p *Player) Start() {
	for {
		select {
		case handlerParam := <-p.HandlerParamCh:
			p.Handler(messageId.MessageId(handlerParam.ID), handlerParam)
		}
	}
}

func (p *Player) Stop() {

}

func (p *Player) OnLogin() {
	//从db加载数据初始化
	//同步数据给客户端

}

func (p *Player) OnLogout() {
	//存db
}
