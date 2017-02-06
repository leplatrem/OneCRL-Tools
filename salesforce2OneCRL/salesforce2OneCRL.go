package main

import (
	"bytes"
	constraintsx509  "constraintcrypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"github.com/mozmark/OneCRL-Tools/oneCRL"
	"os"
	"strings"
	"github.com/mozmark/OneCRL-Tools/salesforce"
	"time"
)

func getDataFromURL(url string) ([]byte, error) {
	r, _ := http.Get(url)
	defer r.Body.Close()

	return ioutil.ReadAll(r.Body)
}

func exists(item string, slice []string) bool {
	for idx := range slice {
		if slice[idx] == item {
			return true
		}
	}
	return false
}

func main() {
	filePtr := flag.String("file", "", "The file to read data from")
	exceptionsPtr := flag.String("exceptions", "exceptions.json", "A JSON document containing exceptional additions")
	outputFmtPtr := flag.String("output", "bug", "The format in which to output data. E.g. 'bug', 'revocations.txt'")
	currentPtr := flag.String("current", "https://firefox.settings.services.mozilla.com/v1/buckets/blocklists/collections/certificates/records", "The URL of the current OneCRL records")
	urlPtr := flag.String("url", "https://mozillacaprogram.secure.force.com/CA/PublicIntermediateCertsRevokedWithPEMCSV", "the URL of the salesforce data")
	flag.Parse()

	var stream io.ReadCloser

	toAdd := make(map[string]oneCRL.Record)

	if "" != *filePtr {
		fmt.Printf("loading salesforce data from %s\n", *filePtr)
		// get the stream from a file
		csvfile, err := os.Open(*filePtr)
		if err != nil {
			fmt.Printf("problem loading salesforce data from file %s\n", err)
			return
		}

		stream = io.ReadCloser(csvfile)
	} else {
		fmt.Printf("loading salesforce data from %s\n", *urlPtr)

		// get the stream from URL
		r, err := http.Get(*urlPtr)
		if err != nil {
			fmt.Printf("problem fetching salesforce data from URL %s\n", err)
			return
		}
		defer r.Body.Close()

		stream = r.Body
	}

	existing, errCurrent := oneCRL.FetchExistingRevocations(*currentPtr)
	if nil != errCurrent {
		fmt.Printf("%s\n", errCurrent)
		//return
	}

	if len(*exceptionsPtr) != 0 {
		res := new(oneCRL.Results)
		data, err := ioutil.ReadFile(*exceptionsPtr)
		if nil != err {
			fmt.Printf("problem loading oneCRL exceptions from file %s\n", err)
		}
		json.Unmarshal(data, res)

		for idx := range res.Data {
			record := res.Data[idx]
			if !exists(oneCRL.StringFromRecord(record), existing) {
				toAdd[oneCRL.StringFromRecord(record)] = record
			}
		}
	}
	
	row := 1
	revoked := salesforce.FetchRevokedCertInfo(stream)

	for _, each := range revoked {
		row++

		if each.Status == "Ready to Add" {
			certData, errPEM := salesforce.CertDataFromSalesforcePEM(each.PEM)
			if errPEM != nil {
				fmt.Printf("(%d, %s, %s) can't decode PEM %s\n", row, each.CSN, each.CertName, each.PEM)
			}

			cert, err2 := constraintsx509.ParseCertificate(certData)
			if err2 != nil {
				fmt.Printf("(%d, %s, %s) could not parse cert\n", row, each.CSN, each.CertName)
				continue
			}

			issuerString := base64.StdEncoding.EncodeToString(cert.RawIssuer)
			serialBytes, err3 := asn1.Marshal(cert.SerialNumber)

			if err3 != nil {
				fmt.Printf("(%d, %s, %s) could not marshal serial number\n", row, each.CSN, each.CertName)
				continue
			}

			serialString := base64.StdEncoding.EncodeToString(serialBytes[2:])

			stringRep := oneCRL.StringFromIssuerSerial(issuerString, serialString)
			if exists(stringRep, existing) {
				fmt.Printf("(%d, %s, %s) revocation already in OneCRL\n", row, each.CSN, each.CertName)
				continue
			}


			matchFound := false
		    lineErrors := ""
			lineWarnings := ""
			for _, CRLLocation := range each.CRLs {
				if 0 != strings.Index(strings.Trim(CRLLocation, " "), "http") {
					if (len(strings.Trim(CRLLocation, " ")) > 0) {
						lineErrors += fmt.Sprintf("Ignoring CRL at %s because it doesn't look like an HTTP url\n", CRLLocation)
					}
					continue
				}

				// fetch and parse the CRL
				res, err4 := http.Get(CRLLocation)
				if err4 != nil {
					lineErrors += fmt.Sprintf("There was a problem fetching the CRL from %s\n", CRLLocation)
					continue
				}

				buf := new(bytes.Buffer)
				_, err6 := buf.ReadFrom(res.Body)
				if err6 != nil {
					fmt.Println("Problem reading the CRL data at %s\n", CRLLocation)
					continue
				}
				crlData := buf.Bytes()

				// Maybe the CRL is PEM - try to parse as PEM and just use the raw
				// data if that fails
				pemBlock, _ := pem.Decode(crlData)
				if pemBlock != nil {
					crlData = pemBlock.Bytes
				}

				crl, err5 := constraintsx509.ParseCRL(crlData)
				if err5 != nil {
					lineErrors += fmt.Sprintf("Could not parse the CRL at \"%s\" %v\n",
											  CRLLocation, err5)
					continue
				}

				// check the CRL is still current
				if crl.HasExpired(time.Now()) {
					lineErrors += fmt.Sprintf("crl %s has expired\n", CRLLocation)
					continue
				}

				// Check the cert issuer and the CRL issuer match
				crlIssuerBytes, errMarshalCRLIssuer := asn1.Marshal(crl.TBSCertList.Issuer)
				if nil != errMarshalCRLIssuer {
					lineErrors += fmt.Sprintf("could not marshal CRL issuer %s\n", CRLLocation)
				}

				readibleCertIssuer, _ := oneCRL.DNToRFC4514(issuerString)
				if ! (oneCRL.ByteArrayEquals(cert.RawIssuer, crlIssuerBytes)) {
					if ! oneCRL.NamesDataMatches(cert.RawIssuer, crlIssuerBytes) {
						lineErrors += fmt.Sprintf("CRL issuer from CRL at %s does not match issuer\n%s !=\n%s\nCRL issuer:  %s\nCert issuer: %s\n", CRLLocation,
						hex.EncodeToString(crlIssuerBytes),
						hex.EncodeToString(cert.RawIssuer),
						oneCRL.RFC4514ish(crl.TBSCertList.Issuer),
						readibleCertIssuer)
						continue
					} else {
						lineWarnings += fmt.Sprintf("Warning: CRL issuer from CRL at %s does not match issuer\n%s !=\n%s\nCRL issuer:  %s\nCert issuer: %s\n", CRLLocation,
						hex.EncodeToString(crlIssuerBytes),
						hex.EncodeToString(cert.RawIssuer),
						oneCRL.RFC4514ish(crl.TBSCertList.Issuer),
						readibleCertIssuer)
					}
				}

				for revoked := range crl.TBSCertList.RevokedCertificates {
					certEntry := crl.TBSCertList.RevokedCertificates[revoked]
					serialBytesFromCRL, _ := asn1.Marshal(certEntry.SerialNumber)
					if oneCRL.ByteArrayEquals(serialBytes, serialBytesFromCRL) {
						matchFound = true
					}
				}
			}
			if !matchFound {
				if lineErrors == "" {
					lineErrors = "\n"
			    }
				fmt.Printf("(%d, %s, %s) no match found in CRL: %s", row, each.CSN, each.CertName, lineErrors)
				continue
			}

			if len(lineWarnings) != 0 {
				fmt.Printf(lineWarnings)
			}

			// record the entry for output later
			rec := oneCRL.Record{}
			rec.IssuerName = issuerString
			rec.SerialNumber = serialString
			toAdd[oneCRL.StringFromRecord(rec)] = rec
		}
	}
	
	issuerMap := make(map[string][]string)

	for _, record := range toAdd {
		if issuers, ok := issuerMap[record.IssuerName]; ok {
			issuerMap[record.IssuerName] = append(issuers, record.SerialNumber)
		} else {
			issuerMap[record.IssuerName] = []string{record.SerialNumber}
		}
	}

	// output the generated entries
	if *outputFmtPtr == "revocations.txt" {
		fmt.Printf("# Auto generated contents. Do not edit.\n")
	}
	for issuer, serials := range issuerMap {
		if *outputFmtPtr == "revocations.txt" {
			fmt.Printf("%v\n", issuer)
		}
		for _, serial := range serials {
			if *outputFmtPtr == "bug" {
				fmt.Printf("issuer: %v serial: %v\n", issuer, serial)
			}
			if *outputFmtPtr == "revocations.txt" {
				fmt.Printf(" %v\n", serial)
			}
		}
	}

}