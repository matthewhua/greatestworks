package main

import "github.com/phuhao00/greatestworks-proto/messageId"

func (c *Client) MessageHandlerRegister() {
	c.messageHandlers[messageId.MessageId_SCCreatePlayer] = c.OnCreatePlayerRsp
	c.messageHandlers[messageId.MessageId_SCLogin] = c.OnLoginRsp
	c.messageHandlers[messageId.MessageId_SCAddFriend] = c.OnAddFriendRsp
	c.messageHandlers[messageId.MessageId_SCDelFriend] = c.OnDelFriendRsp
	c.messageHandlers[messageId.MessageId_SCSendChatMsg] = c.OnSendChatMsgRsp
}
