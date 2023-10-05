// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2018 Canonical Ltd
// Copyright (C) 2018 IOTech Ltd
// Copyright (C) 2021 Schneider Electric
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edgexfoundry/device-opcua-go/internal/config"
	sdkModels "github.com/edgexfoundry/device-sdk-go/v3/pkg/models"
	"github.com/edgexfoundry/go-mod-core-contracts/v3/models"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

func (d *Driver) startSubscriptionListener() error {

	var (
		deviceName = d.serviceConfig.OPCUAServer.DeviceName
		resources  = d.serviceConfig.OPCUAServer.Writable.Resources
	)

	// No need to start a subscription if there are no resources to monitor
	if len(resources) == 0 {
		d.Logger.Info("[Incoming listener] No resources defined to generate subscriptions.")
		return nil
	} else {
		d.Logger.Infof("Resources: %v", resources)
	}

	// Create a cancelable context for Writable configuration
	ctxBg := context.Background()
	ctx, cancel := context.WithCancel(ctxBg)
	d.ctxCancel = cancel

	// ds := service.RunningService()
	// if ds == nil {
	// 	return fmt.Errorf("[Incoming listener] unable to get running device service")
	// }
	d.Logger.Infof("GetDeviceByName: %v", resources)
	device, err := d.sdk.GetDeviceByName(deviceName)
	if err != nil {
		return err
	}
	d.Logger.Infof("getClient: %v", resources)
	client, err := d.getClient(device)
	if err != nil {
		return err
	}
	d.Logger.Infof("client.Connect: %v", resources)
	if err := client.Connect(ctx); err != nil {
		d.Logger.Warnf("[Incoming listener] Failed to connect OPCUA client, %s", err)
		return err
	}
	defer client.Close()

	notificationChannel := make(chan *opcua.PublishNotificationData)

	d.Logger.Infof("SubscribeWithContext: %v", resources)
	sub, err := client.SubscribeWithContext(ctx, &opcua.SubscriptionParameters{
		Interval: time.Duration(500) * time.Millisecond,
	}, notificationChannel)
	if err != nil {
		return err
	}
	defer sub.Cancel(ctx)
	d.Logger.Infof("Created subscription with id %v", sub.SubscriptionID)

	if err := d.configureMonitoredItems(sub, resources, deviceName); err != nil {
		return err
	}

	// read from subscription's notification channel until ctx is cancelled
	// stackTrace := make([]byte, 1024)
	// length := runtime.Stack(stackTrace, false)
	// fmt.Printf("Stack Trace of Current Goroutine:\n%s\n", stackTrace[:length])
	for {
		select {
		case <-ctx.Done():
			return nil
		case res := <-notificationChannel:
			if res.Error != nil {
				d.Logger.Debug(res.Error.Error())
				continue
			}
			switch x := res.Value.(type) {
			case *ua.DataChangeNotification:
				d.handleDataChange(x)
			default:
				d.Logger.Infof("what's this publish result? %T", res.Value)
			}
		}
	}
}

func (d *Driver) getClient(device models.Device) (*opcua.Client, error) {
	var (
		policy   = d.serviceConfig.OPCUAServer.Policy
		mode     = d.serviceConfig.OPCUAServer.Mode
		certFile = d.serviceConfig.OPCUAServer.CertFile
		keyFile  = d.serviceConfig.OPCUAServer.KeyFile
	)

	username, password, xerr := config.FetchAuthUsername(device.Protocols)
	if xerr != nil || (username == "" || password == "") {
		return nil, xerr
	}
	// fmt.Printf("Username : %s, Password : %s\n", username, password)
	endpoint, xerr := config.FetchEndpoint(device.Protocols)
	if xerr != nil {
		return nil, xerr
	}
	ctx := context.Background()
	endpoints, err := opcua.GetEndpoints(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	ep := opcua.SelectEndpoint(endpoints, policy, ua.MessageSecurityModeFromString(mode))
	if ep == nil {
		return nil, fmt.Errorf("[Incoming listener] Failed to find suitable endpoint")
	}
	ep.EndpointURL = endpoint

	opts := []opcua.Option{
		opcua.SecurityPolicy(policy),
		opcua.SecurityModeString(mode),
		opcua.CertificateFile(certFile),
		opcua.PrivateKeyFile(keyFile),
		// opcua.AuthAnonymous(),
		opcua.AuthUsername(username, password),
		opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeUserName),
		opcua.AutoReconnect(true),
		opcua.ReconnectInterval(time.Second * 10),
	}

	return opcua.NewClient(ep.EndpointURL, opts...), nil
}

func (d *Driver) configureMonitoredItems(sub *opcua.Subscription, resources, deviceName string) error {
	// ds := service.RunningService()
	// if ds == nil {
	// 	return fmt.Errorf("[Incoming listener] unable to get running device service")
	// }

	d.mu.Lock()
	defer d.mu.Unlock()

	for i, node := range strings.Split(resources, ",") {
		deviceResource, ok := d.sdk.DeviceResource(deviceName, node)
		if !ok {
			return fmt.Errorf("[Incoming listener] Unable to find device resource with name %s", node)
		}

		opcuaNodeID, err := getNodeID(deviceResource.Attributes, NODE)
		if err != nil {
			return err
		}

		id, err := ua.ParseNodeID(opcuaNodeID)
		if err != nil {
			return err
		}

		// arbitrary client handle for the monitoring item
		handle := uint32(i + 42)
		// map the client handle so we know what the value returned represents
		d.resourceMap[handle] = node
		miCreateRequest := opcua.NewMonitoredItemCreateRequestWithDefaults(id, ua.AttributeIDValue, handle)
		res, err := sub.Monitor(ua.TimestampsToReturnBoth, miCreateRequest)
		if err != nil || res.Results[0].StatusCode != ua.StatusOK {
			return err
		}

		d.Logger.Infof("[Incoming listener] Start incoming data listening for %s.", node)
	}

	return nil
}

func (d *Driver) handleDataChange(dcn *ua.DataChangeNotification) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, item := range dcn.MonitoredItems {
		data := item.Value.Value.Value()
		nodeName := d.resourceMap[item.ClientHandle]
		if err := d.onIncomingDataReceived(data, nodeName); err != nil {
			d.Logger.Infof("MonitoredItem with client handle %v = %v", nodeName, data)
			d.Logger.Errorf("%v", err)
		}
	}
}

func (d *Driver) onIncomingDataReceived(data interface{}, nodeResourceName string) error {
	deviceName := d.serviceConfig.OPCUAServer.DeviceName
	reading := data

	deviceResource, ok := d.sdk.DeviceResource(deviceName, nodeResourceName)
	if !ok {
		d.Logger.Warnf("[Incoming listener] Incoming reading ignored. No DeviceObject found: name=%v deviceResource=%v value=%v", deviceName, nodeResourceName, data)
		return nil
	}

	req := sdkModels.CommandRequest{
		DeviceResourceName: nodeResourceName,
		Type:               deviceResource.Properties.ValueType,
	}

	result, err := newResult(req, reading)
	if err != nil {
		d.Logger.Warnf("[Incoming listener] Incoming reading ignored. name=%v deviceResource=%v value=%v", deviceName, nodeResourceName, data)
		return nil
	}

	asyncValues := &sdkModels.AsyncValues{
		DeviceName:    deviceName,
		CommandValues: []*sdkModels.CommandValue{result},
	}

	d.Logger.Infof("[Incoming listener] Incoming reading received: name=%v deviceResource=%v value=%v", deviceName, nodeResourceName, data)

	d.AsyncCh <- asyncValues

	return nil
}

func valueRequest(nodeID *ua.NodeID) *ua.MonitoredItemCreateRequest {
	handle := uint32(42)
	return opcua.NewMonitoredItemCreateRequestWithDefaults(nodeID, ua.AttributeIDValue, handle)
}

func eventRequest(nodeID *ua.NodeID) (*ua.MonitoredItemCreateRequest, []string) {
	fieldNames := []string{"EventId", "EventType", "Severity", "Time", "Message"}
	selects := make([]*ua.SimpleAttributeOperand, len(fieldNames))

	for i, name := range fieldNames {
		selects[i] = &ua.SimpleAttributeOperand{
			TypeDefinitionID: ua.NewNumericNodeID(0, id.BaseEventType),
			BrowsePath:       []*ua.QualifiedName{{NamespaceIndex: 0, Name: name}},
			AttributeID:      ua.AttributeIDValue,
		}
	}

	wheres := &ua.ContentFilter{
		Elements: []*ua.ContentFilterElement{
			{
				FilterOperator: ua.FilterOperatorGreaterThanOrEqual,
				FilterOperands: []*ua.ExtensionObject{
					{
						EncodingMask: 1,
						TypeID: &ua.ExpandedNodeID{
							NodeID: ua.NewNumericNodeID(0, id.SimpleAttributeOperand_Encoding_DefaultBinary),
						},
						Value: ua.SimpleAttributeOperand{
							TypeDefinitionID: ua.NewNumericNodeID(0, id.BaseEventType),
							BrowsePath:       []*ua.QualifiedName{{NamespaceIndex: 0, Name: "Severity"}},
							AttributeID:      ua.AttributeIDValue,
						},
					},
					{
						EncodingMask: 1,
						TypeID: &ua.ExpandedNodeID{
							NodeID: ua.NewNumericNodeID(0, id.LiteralOperand_Encoding_DefaultBinary),
						},
						Value: ua.LiteralOperand{
							Value: ua.MustVariant(uint16(0)),
						},
					},
				},
			},
		},
	}

	filter := ua.EventFilter{
		SelectClauses: selects,
		WhereClause:   wheres,
	}

	filterExtObj := ua.ExtensionObject{
		EncodingMask: ua.ExtensionObjectBinary,
		TypeID: &ua.ExpandedNodeID{
			NodeID: ua.NewNumericNodeID(0, id.EventFilter_Encoding_DefaultBinary),
		},
		Value: filter,
	}

	handle := uint32(42)
	req := &ua.MonitoredItemCreateRequest{
		ItemToMonitor: &ua.ReadValueID{
			NodeID:       nodeID,
			AttributeID:  ua.AttributeIDEventNotifier,
			DataEncoding: &ua.QualifiedName{},
		},
		MonitoringMode: ua.MonitoringModeReporting,
		RequestedParameters: &ua.MonitoringParameters{
			ClientHandle:     handle,
			DiscardOldest:    true,
			Filter:           &filterExtObj,
			QueueSize:        10,
			SamplingInterval: 1.0,
		},
	}

	return req, fieldNames
}
