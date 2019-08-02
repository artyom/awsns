// Command awsns creates/updates A/CNAME records for given DNS zone in AWS Route
// 53 for all running non-spot ec2 instances based on their "Name" tag.
//
// It can be run manually with -zone flag set to hosted zone ID and -suffix set
// to suffix to construct domain names from. Domain names constructed by
// concatenating value of the "Name" instance tag with suffix value set by
// -suffix flag.
//
// I.e. if ec2 instance tag "Name" is set to "jenkins" and program is called
// with -suffix=".foo.example.com", then constructed name would be
// jenkins.foo.example.com. Zone ID must match either example.com or
// foo.example.com zone in Route 53 console.
//
// Program may remove existing A/CNAME records matching given suffix if no
// corresponding non-spot ec2 instances found running.
//
// If ec2 instance has public DNS name, program creates CNAME record pointing to
// such name, otherwise it creates A record pointing to public IP address.
//
// Program may also be run as AWS Lambda invoked by CloudWatch event created as
// "EC2 Instance State-change Notification" for "running" state. It then looks
// up suffix and zone id in SUFFIX and ZONE environment variables. Lambda needs
// permissions to describe EC2 instances and list/update Route 53 records;
// required permissions can be satisfied by using the following AWS managed
// policies: AmazonEC2ReadOnlyAccess, AmazonRoute53FullAccess,
// AWSLambdaBasicExecutionRole.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/artyom/autoflags"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/route53"
)

func main() {
	if os.Getenv("LAMBDA_TASK_ROOT") != "" && os.Getenv("AWS_EXECUTION_ENV") != "" {
		lambda.Start(func(ctx context.Context, evt events.CloudWatchEvent) error {
			if evt.Source != "aws.ec2" {
				log.Printf("unsupported event source: %q", evt.Source)
				return nil
			}
			det := struct {
				ID    string `json:"instance-id"`
				State string `json:"state"`
			}{}
			if err := json.Unmarshal(evt.Detail, &det); err != nil {
				return err
			}
			if det.State != "running" {
				log.Printf("unsupported ec2 instance state: %q", det.State)
				return nil
			}
			if det.ID == "" {
				log.Println("empty instance id")
				return nil
			}
			return run(ctx, os.Getenv("SUFFIX"), os.Getenv("ZONE"), det.ID)
		})
		return
	}
	args := struct {
		Suffix string `flag:"suffix,dns zone suffix, i.e. .subdomain.example.com"`
		Zone   string `flag:"zone,Route 53 hosted zone id"`
	}{}
	autoflags.Parse(&args)
	if err := run(context.Background(), args.Suffix, args.Zone, ""); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, suffix, zoneID, invokerID string) error {
	if suffix == "." || !strings.HasPrefix(suffix, ".") {
		return fmt.Errorf("invalid suffix %q, must start with dot, like '.example.com'", suffix)
	}
	if zoneID == "" {
		return fmt.Errorf("hosted zone id cannot be empty")
	}
	sess, err := session.NewSession()
	if err != nil {
		return err
	}
	instances, err := runningInstances(ctx, ec2.New(sess))
	if err != nil {
		return err
	}
	if invokerID != "" {
		var found bool
		for _, inst := range instances {
			if inst.InstanceId != nil && *inst.InstanceId == invokerID {
				found = true
				break
			}
		}
		// return rigth away if invoked by spot instance launch
		if !found {
			return nil
		}
	}
	r53svc := route53.New(sess)
	toRemove := make(map[string]*route53.Change)
	fn := func(page *route53.ListResourceRecordSetsOutput, lastPage bool) bool {
		suffix := suffix + "."
		for _, rr := range page.ResourceRecordSets {
			if rr.Name == nil || *rr.Name == suffix || !strings.HasSuffix(*rr.Name, suffix) {
				continue
			}
			if rr.Type == nil || (*rr.Type != "A" && *rr.Type != "CNAME") {
				continue
			}
			name := strings.TrimSuffix(*rr.Name, ".")
			toRemove[name] = &route53.Change{
				Action: aws.String("DELETE"),
				ResourceRecordSet: &route53.ResourceRecordSet{
					Name:            &name,
					TTL:             rr.TTL,
					Type:            rr.Type,
					ResourceRecords: rr.ResourceRecords,
				},
			}
		}
		return true
	}
	listInput := &route53.ListResourceRecordSetsInput{
		HostedZoneId: &zoneID,
	}
	if err := r53svc.ListResourceRecordSetsPagesWithContext(ctx, listInput, fn); err != nil {
		return err
	}
	log.Println("removal candidates:", len(toRemove))
	var changes []*route53.Change
	for _, inst := range instances {
		var name string
		for _, tag := range inst.Tags {
			if *tag.Key == "Name" {
				name = *tag.Value
				break
			}
		}
		if !valid(name) {
			continue
		}
		ch := &route53.Change{
			Action: aws.String("UPSERT"),
			ResourceRecordSet: &route53.ResourceRecordSet{
				Name: aws.String(name + suffix),
				TTL:  aws.Int64(60),
			},
		}
		switch {
		case inst.PublicDnsName != nil && *inst.PublicDnsName != "":
			ch.ResourceRecordSet.Type = aws.String("CNAME")
			ch.ResourceRecordSet.ResourceRecords = []*route53.ResourceRecord{{
				Value: aws.String(*inst.PublicDnsName),
			}}
		case inst.PublicIpAddress != nil && *inst.PublicIpAddress != "":
			ch.ResourceRecordSet.Type = aws.String("A")
			ch.ResourceRecordSet.ResourceRecords = []*route53.ResourceRecord{{
				Value: aws.String(*inst.PublicIpAddress),
			}}
		default:
			continue
		}
		delete(toRemove, name+suffix)
		changes = append(changes, ch)
	}
	if len(changes) == 0 {
		return fmt.Errorf("no changes to apply")
	}
	log.Println("actually removing:", len(toRemove))
	for name, ch := range toRemove {
		log.Println("removing:", name)
		changes = append(changes, ch)
	}
	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: &zoneID,
		ChangeBatch: &route53.ChangeBatch{
			Changes: changes,
			Comment: aws.String("automated update for running instances"),
		},
	}
	_, err = r53svc.ChangeResourceRecordSetsWithContext(ctx, input)
	return err
}

func runningInstances(ctx context.Context, svc *ec2.EC2) ([]*ec2.Instance, error) {
	resp, err := svc.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []*string{aws.String("running")},
		}},
	})
	if err != nil {
		return nil, err
	}
	var out []*ec2.Instance
	for _, r := range resp.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceLifecycle != nil {
				continue // skip spot instances
			}
			out = append(out, inst)
		}
	}
	return out, nil
}

func valid(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case 'a' <= r && r <= 'z':
		case 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

func init() { log.SetFlags(0) }
