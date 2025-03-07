// Copyright 2021 Confluent Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"context"
	"fmt"
	"github.com/antihax/optional"
	kafkarestv3 "github.com/confluentinc/ccloud-sdk-go-v2/kafkarest/v3"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"log"
	"net/http"
	"regexp"
	"strings"
)

const (
	paramResourceName = "resource_name"
	paramResourceType = "resource_type"
	paramPatternType  = "pattern_type"
	paramPrincipal    = "principal"
	paramHost         = "host"
	paramOperation    = "operation"
	paramPermission   = "permission"

	principalPrefix = "User:"
)

var acceptedResourceTypes = []string{"UNKNOWN", "ANY", "TOPIC", "GROUP", "CLUSTER", "TRANSACTIONAL_ID", "DELEGATION_TOKEN"}
var acceptedPatternTypes = []string{"UNKNOWN", "ANY", "MATCH", "LITERAL", "PREFIXED"}
var acceptedOperations = []string{"UNKNOWN", "ANY", "ALL", "READ", "WRITE", "CREATE", "DELETE", "ALTER", "DESCRIBE", "CLUSTER_ACTION", "DESCRIBE_CONFIGS", "ALTER_CONFIGS", "IDEMPOTENT_WRITE"}
var acceptedPermissions = []string{"UNKNOWN", "ANY", "DENY", "ALLOW"}

func extractAcl(d *schema.ResourceData) (Acl, error) {
	resourceType, err := stringToAclResourceType(d.Get(paramResourceType).(string))
	if err != nil {
		return Acl{}, err
	}
	patternType, err := stringToAclPatternType(d.Get(paramPatternType).(string))
	if err != nil {
		return Acl{}, err
	}
	operation, err := stringToAclOperation(d.Get(paramOperation).(string))
	if err != nil {
		return Acl{}, err
	}
	permission, err := stringToAclPermission(d.Get(paramPermission).(string))
	if err != nil {
		return Acl{}, err
	}
	return Acl{
		ResourceType: resourceType,
		ResourceName: d.Get(paramResourceName).(string),
		PatternType:  patternType,
		Principal:    d.Get(paramPrincipal).(string),
		Host:         d.Get(paramHost).(string),
		Operation:    operation,
		Permission:   permission,
	}, nil
}

func kafkaAclResource() *schema.Resource {
	return &schema.Resource{
		CreateContext: kafkaAclCreate,
		ReadContext:   kafkaAclRead,
		UpdateContext: kafkaAclUpdate,
		DeleteContext: kafkaAclDelete,
		Importer: &schema.ResourceImporter{
			StateContext: kafkaAclImport,
		},
		Schema: map[string]*schema.Schema{
			paramClusterId: clusterIdSchema(),
			paramResourceType: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				Description:  "The type of the resource.",
				ValidateFunc: validation.StringInSlice(acceptedResourceTypes, false),
			},
			paramResourceName: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The resource name for the ACL.",
			},
			paramPatternType: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				Description:  "The pattern type for the ACL.",
				ValidateFunc: validation.StringInSlice(acceptedPatternTypes, false),
			},
			paramPrincipal: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				Description:  "The principal for the ACL.",
				ValidateFunc: validation.StringMatch(regexp.MustCompile("^User:sa-"), "the principal must start with 'User:sa-'. Follow the upgrade guide at https://registry.terraform.io/providers/confluentinc/confluentcloud/latest/docs/guides/upgrade-guide-0.4.0 to upgrade to the latest version of Terraform Provider for Confluent Cloud"),
			},
			paramHost: {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "*",
				ForceNew:    true,
				Description: "The host for the ACL.",
			},
			paramOperation: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				Description:  "The operation type for the ACL.",
				ValidateFunc: validation.StringInSlice(acceptedOperations, false),
			},
			paramPermission: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				Description:  "The permission for the ACL.",
				ValidateFunc: validation.StringInSlice(acceptedPermissions, false),
			},
			paramHttpEndpoint: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The REST endpoint of the Kafka cluster (e.g., `https://pkc-00000.us-central1.gcp.confluent.cloud:443`).",
			},
			paramCredentials: credentialsSchema(),
		},
	}
}

func kafkaAclCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	httpEndpoint := d.Get(paramHttpEndpoint).(string)
	clusterId := d.Get(paramClusterId).(string)
	clusterApiKey, clusterApiSecret, err := extractClusterApiKeyAndApiSecret(d)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}
	kafkaRestClient := meta.(*Client).kafkaRestClientFactory.CreateKafkaRestClient(httpEndpoint, clusterId, clusterApiKey, clusterApiSecret)
	acl, err := extractAcl(d)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}
	// APIF-2038: Kafka REST API only accepts integer ID at the moment
	c := meta.(*Client)
	principalWithIntegerId, err := principalWithResourceIdToPrincipalWithIntegerId(c, acl.Principal)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}
	kafkaAclRequestData := kafkarestv3.CreateAclRequestData{
		ResourceType: acl.ResourceType,
		ResourceName: acl.ResourceName,
		PatternType:  acl.PatternType,
		Principal:    principalWithIntegerId,
		Host:         acl.Host,
		Operation:    acl.Operation,
		Permission:   acl.Permission,
	}

	resp, err := executeKafkaAclCreate(ctx, kafkaRestClient, kafkaAclRequestData)

	if err != nil {
		log.Printf("[ERROR] Kafka ACL create failed %v, %v, %s", kafkaAclRequestData, resp, err)
		return createDiagnosticsWithDetails(err)
	}
	kafkaAclId := createKafkaAclId(kafkaRestClient.clusterId, acl)
	d.SetId(kafkaAclId)
	log.Printf("[DEBUG] Created kafka ACL %s", kafkaAclId)
	return kafkaAclRead(ctx, d, meta)
}

func executeKafkaAclCreate(ctx context.Context, c *KafkaRestClient, requestData kafkarestv3.CreateAclRequestData) (*http.Response, error) {
	opts := &kafkarestv3.CreateKafkaV3AclsOpts{
		CreateAclRequestData: optional.NewInterface(requestData),
	}
	return c.apiClient.ACLV3Api.CreateKafkaV3Acls(c.apiContext(ctx), c.clusterId, opts)
}

func kafkaAclDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	log.Printf("[INFO] Kafka ACL delete for %s", d.Id())

	httpEndpoint := d.Get(paramHttpEndpoint).(string)
	clusterId := d.Get(paramClusterId).(string)
	clusterApiKey, clusterApiSecret, err := extractClusterApiKeyAndApiSecret(d)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}
	kafkaRestClient := meta.(*Client).kafkaRestClientFactory.CreateKafkaRestClient(httpEndpoint, clusterId, clusterApiKey, clusterApiSecret)

	acl, err := extractAcl(d)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}

	// APIF-2038: Kafka REST API only accepts integer ID at the moment
	client := meta.(*Client)
	principalWithIntegerId, err := principalWithResourceIdToPrincipalWithIntegerId(client, acl.Principal)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}

	opts := &kafkarestv3.DeleteKafkaV3AclsOpts{
		ResourceType: optional.NewInterface(acl.ResourceType),
		ResourceName: optional.NewString(acl.ResourceName),
		PatternType:  optional.NewInterface(acl.PatternType),
		Principal:    optional.NewString(principalWithIntegerId),
		Host:         optional.NewString(acl.Host),
		Operation:    optional.NewInterface(acl.Operation),
		Permission:   optional.NewInterface(acl.Permission),
	}

	_, _, err = kafkaRestClient.apiClient.ACLV3Api.DeleteKafkaV3Acls(kafkaRestClient.apiContext(ctx), kafkaRestClient.clusterId, opts)

	if err != nil {
		return diag.Errorf("error deleting kafka ACL (%s), err: %s", d.Id(), err)
	}

	return nil
}

func executeKafkaAclRead(ctx context.Context, c *KafkaRestClient, opts *kafkarestv3.GetKafkaV3AclsOpts) (kafkarestv3.AclDataList, *http.Response, error) {
	return c.apiClient.ACLV3Api.GetKafkaV3Acls(c.apiContext(ctx), c.clusterId, opts)
}

func kafkaAclRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	log.Printf("[INFO] Kafka ACL read for %s", d.Id())

	httpEndpoint := d.Get(paramHttpEndpoint).(string)
	clusterId := d.Get(paramClusterId).(string)
	clusterApiKey, clusterApiSecret, err := extractClusterApiKeyAndApiSecret(d)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}
	client := meta.(*Client)
	kafkaRestClient := meta.(*Client).kafkaRestClientFactory.CreateKafkaRestClient(httpEndpoint, clusterId, clusterApiKey, clusterApiSecret)
	acl, err := extractAcl(d)
	if err != nil {
		return createDiagnosticsWithDetails(err)
	}

	// APIF-2043: TEMPORARY CODE for v0.x.0 -> v0.4.0 migration
	// Destroy the resource in terraform state if it uses integerId for a principal.
	// This hack is necessary since terraform plan will use the principal's value (integerId) from terraform.state
	// instead of using the new provided resourceId from main.tf (the user will be forced to replace integerId with resourceId
	// that we have an input validation for using "User:sa-" for principal attribute.
	if !strings.HasPrefix(acl.Principal, "User:sa-") {
		d.SetId("")
		return nil
	}

	_, err = readAndSetAclResourceConfigurationArguments(ctx, d, client, kafkaRestClient, acl)

	return createDiagnosticsWithDetails(err)
}

func createKafkaAclId(clusterId string, acl Acl) string {
	return fmt.Sprintf("%s/%s", clusterId, strings.Join([]string{
		string(acl.ResourceType),
		acl.ResourceName,
		string(acl.PatternType),
		acl.Principal,
		acl.Host,
		string(acl.Operation),
		string(acl.Permission),
	}, "#"))
}

func readAndSetAclResourceConfigurationArguments(ctx context.Context, d *schema.ResourceData, client *Client, c *KafkaRestClient, acl Acl) ([]*schema.ResourceData, error) {
	// APIF-2038: Kafka REST API only accepts integer ID at the moment
	principalWithIntegerId, err := principalWithResourceIdToPrincipalWithIntegerId(client, acl.Principal)
	if err != nil {
		return nil, err
	}

	opts := &kafkarestv3.GetKafkaV3AclsOpts{
		ResourceType: optional.NewInterface(acl.ResourceType),
		ResourceName: optional.NewString(acl.ResourceName),
		PatternType:  optional.NewInterface(acl.PatternType),
		Principal:    optional.NewString(principalWithIntegerId),
		Host:         optional.NewString(acl.Host),
		Operation:    optional.NewInterface(acl.Operation),
		Permission:   optional.NewInterface(acl.Permission),
	}

	remoteAcls, resp, err := executeKafkaAclRead(ctx, c, opts)
	if err != nil {
		log.Printf("[ERROR] Kafka ACL get failed for id %s, %v, %s", acl, resp, err)

		// https://learn.hashicorp.com/tutorials/terraform/provider-setup
		isResourceNotFound := HasStatusNotFound(resp)
		if isResourceNotFound {
			log.Printf("[WARN] Kafka ACL with id=%s is not found", d.Id())
			// If the resource isn't available, Terraform destroys the resource in state.
			d.SetId("")
			return nil, nil
		}

		return nil, err
	}
	if len(remoteAcls.Data) == 0 {
		return nil, fmt.Errorf("no Kafka ACLs were matched for id=%s", d.Id())
	} else if len(remoteAcls.Data) > 1 {
		return nil, fmt.Errorf("multiple Kafka ACLs were matched for id=%s: %v", d.Id(), remoteAcls.Data)
	}
	matchedAcl := remoteAcls.Data[0]
	if err := d.Set(paramClusterId, c.clusterId); err != nil {
		return nil, err
	}
	if err := d.Set(paramResourceType, matchedAcl.ResourceType); err != nil {
		return nil, err
	}
	if err := d.Set(paramResourceName, matchedAcl.ResourceName); err != nil {
		return nil, err
	}
	if err := d.Set(paramPatternType, matchedAcl.PatternType); err != nil {
		return nil, err
	}
	// Use principal with resource ID
	if err := d.Set(paramPrincipal, acl.Principal); err != nil {
		return nil, err
	}
	if err := d.Set(paramHost, matchedAcl.Host); err != nil {
		return nil, err
	}
	if err := d.Set(paramOperation, matchedAcl.Operation); err != nil {
		return nil, err
	}
	if err := d.Set(paramPermission, matchedAcl.Permission); err != nil {
		return nil, err
	}
	if err := setKafkaCredentials(c.clusterApiKey, c.clusterApiSecret, d); err != nil {
		return nil, err
	}
	if err := d.Set(paramHttpEndpoint, c.httpEndpoint); err != nil {
		return nil, err
	}
	d.SetId(createKafkaAclId(c.clusterId, acl))
	return []*schema.ResourceData{d}, err
}

func kafkaAclImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	log.Printf("[INFO] Kafka ACL import for %s", d.Id())

	kafkaImportEnvVars, err := checkEnvironmentVariablesForKafkaImportAreSet()
	if err != nil {
		return nil, err
	}

	clusterIdAndSerializedAcl := d.Id()

	parts := strings.Split(clusterIdAndSerializedAcl, "/")

	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid format for kafka ACL import: expected '<lkc ID>/<resource type>#<resource name>#<pattern type>#<principal>#<host>#<operation>#<permission>'")
	}

	clusterId := parts[0]
	serializedAcl := parts[1]

	acl, err := deserializeAcl(serializedAcl)
	if err != nil {
		return nil, err
	}

	client := meta.(*Client)
	kafkaRestClient := meta.(*Client).kafkaRestClientFactory.CreateKafkaRestClient(kafkaImportEnvVars.kafkaHttpEndpoint, clusterId, kafkaImportEnvVars.kafkaApiKey, kafkaImportEnvVars.kafkaApiSecret)

	return readAndSetAclResourceConfigurationArguments(ctx, d, client, kafkaRestClient, acl)
}

func deserializeAcl(serializedAcl string) (Acl, error) {
	parts := strings.Split(serializedAcl, "#")
	if len(parts) != 7 {
		return Acl{}, fmt.Errorf("invalid format for kafka ACL import: expected '<lkc ID>/<resource type>#<resource name>#<pattern type>#<principal>#<host>#<operation>#<permission>'")
	}

	resourceType, err := stringToAclResourceType(parts[0])
	if err != nil {
		return Acl{}, err
	}
	patternType, err := stringToAclPatternType(parts[2])
	if err != nil {
		return Acl{}, err
	}
	operation, err := stringToAclOperation(parts[5])
	if err != nil {
		return Acl{}, err
	}
	permission, err := stringToAclPermission(parts[6])
	if err != nil {
		return Acl{}, err
	}

	return Acl{
		ResourceType: resourceType,
		ResourceName: parts[1],
		PatternType:  patternType,
		Principal:    parts[3],
		Host:         parts[4],
		Operation:    operation,
		Permission:   permission,
	}, nil
}

func kafkaAclUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	if d.HasChangesExcept(paramCredentials) {
		return diag.Errorf("only %s block can be updated for a Kafka ACL", paramCredentials)
	}
	return kafkaAclRead(ctx, d, meta)
}
