package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestExecuteSOAPMissingWSDL(t *testing.T) {
	dispatcher := NewDispatcher()
	srv := &models.Service{}
	obj := &models.IntegrationObject{
		Method: "SOAP",
	}

	stream := NewBufferStream()
	status, err := dispatcher.executeSOAP(context.Background(), srv, obj, nil, nil, nil, stream)

	if status != 500 {
		t.Errorf("expected 500 status, got %d", status)
	}

	if err == nil || err.Error() != "missing RawWSDL on service to execute SOAP request" {
		t.Errorf("expected missing RawWSDL error, got %v", err)
	}
}

func TestExecuteSOAPAcquiresBeforeProviderCall(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	dispatcher := &Dispatcher{rateLimits: rateStore, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><GetInvoiceResponse xmlns="urn:test"><result>ok</result></GetInvoiceResponse></soap:Body></soap:Envelope>`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}}
	srv := &models.Service{RawWSDL: testSOAPWSDL, ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	obj := &models.IntegrationObject{Method: "SOAP", NormalizedPath: "GetInvoice", StableKey: "soap:GetInvoice"}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	status, err := dispatcher.executeSOAP(ctx, srv, obj, map[string]any{"id": "1"}, nil, nil, NewBufferStream())
	if err != nil || status != http.StatusOK || len(rateStore.requests) != 1 {
		t.Fatalf("status=%d err=%v acquisitions=%d", status, err, len(rateStore.requests))
	}
}

const testSOAPWSDL = `<?xml version="1.0"?>
<wsdl:definitions xmlns:wsdl="http://schemas.xmlsoap.org/wsdl/" xmlns:soap="http://schemas.xmlsoap.org/wsdl/soap/" xmlns:tns="urn:test" xmlns:xsd="http://www.w3.org/2001/XMLSchema" targetNamespace="urn:test">
  <wsdl:types><xsd:schema targetNamespace="urn:test"><xsd:element name="GetInvoice"><xsd:complexType><xsd:sequence><xsd:element name="id" type="xsd:string"/></xsd:sequence></xsd:complexType></xsd:element><xsd:element name="GetInvoiceResponse"><xsd:complexType><xsd:sequence><xsd:element name="result" type="xsd:string"/></xsd:sequence></xsd:complexType></xsd:element></xsd:schema></wsdl:types>
  <wsdl:message name="GetInvoiceInput"><wsdl:part name="parameters" element="tns:GetInvoice"/></wsdl:message>
  <wsdl:message name="GetInvoiceOutput"><wsdl:part name="parameters" element="tns:GetInvoiceResponse"/></wsdl:message>
  <wsdl:portType name="InvoicePortType"><wsdl:operation name="GetInvoice"><wsdl:input message="tns:GetInvoiceInput"/><wsdl:output message="tns:GetInvoiceOutput"/></wsdl:operation></wsdl:portType>
  <wsdl:binding name="InvoiceBinding" type="tns:InvoicePortType"><soap:binding style="document" transport="http://schemas.xmlsoap.org/soap/http"/><wsdl:operation name="GetInvoice"><soap:operation soapAction="GetInvoice"/><wsdl:input><soap:body use="literal"/></wsdl:input><wsdl:output><soap:body use="literal"/></wsdl:output></wsdl:operation></wsdl:binding>
  <wsdl:service name="InvoiceService"><wsdl:port name="InvoicePort" binding="tns:InvoiceBinding"><soap:address location="https://provider.test/soap"/></wsdl:port></wsdl:service>
</wsdl:definitions>`
