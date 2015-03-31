package discovery // config

import (
	"fmt"
	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd"
	clientcmdapi "github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubectl/resource"
	"github.com/openshift/openshift-extras/diagnostics/log"
	"github.com/openshift/openshift-extras/diagnostics/types"
	"github.com/openshift/origin/pkg/cmd/cli/config"
	osclientcmd "github.com/openshift/origin/pkg/cmd/util/clientcmd"
	pkgapi "github.com/openshift/origin/pkg/project/api"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	//"github.com/openshift/origin/pkg/cmd/util"
)

/* ----------------------------------------------------------
Look for the client config and try to read it.

We will look in the standard locations, alert the user to what we find
as we go along, and try to be helpful.
*/

// -------------------------------------------------------------
// Look for client config file in a number of possible locations
func readClientConfigFiles(env *types.Environment) {
	var found bool
	confFlag := []string{env.Flags.OpenshiftFlags.Lookup(config.OpenShiftConfigFlagName).Value.String()}
	for index, path := range append(confFlag, config.FullClientConfigFilePriority()...) {
		errmsg := ""
		switch index {
		case 0:
			errmsg = fmt.Sprintf("--config specified that client config should be at %s\n", path)
		case 1:
			errmsg = fmt.Sprintf("$OPENSHIFTCONFIG specified that client config should be at %s\n", path)
		}
		if rawConfig := openConfigFile(path, errmsg); rawConfig != nil && !found {
			found = true
			env.ClientConfigPath = path
			env.ClientConfigRaw = rawConfig
		}
	}
	if !found {
		log.Warn("discNoCC", "No client config file read; default OpenShift config will be used, which is not likely what you want.")
		adminPaths := []string{
			"/var/lib/openshift/openshift.certificates.d/admin/.kubeconfig", // enterprise
			"/openshift.certificates.d/admin/.kubeconfig",                   // origin systemd
			"./openshift.certificates.d/admin/.kubeconfig",                  // origin binary
		}
		adminWarningF := `
Client config was not available where expected; however, one exists at
  %s
which is a standard location where the master generates it.
If this is what you want, you should copy it to a standard location
(~/.config/openshift/.config, or the current directory), or you can set the
environment variable OPENSHIFTCONFIG in your ~/.bash_profile:
  export OPENSHIFTCONFIG=%[1]s
If this is not what you want, you should obtain a config file and
place it in a standard location.
`
		// look for it in auto-generated locations when not found properly
		for _, path := range adminPaths {
			if conf := openConfigFile(path, ""); conf != nil {
				log.Warnf("discCCautoPath", adminWarningF, path)
				break
			}
		}
	}
}

// ----------------------------------------------------------
// Attempt to open file at path as client config
// If there is a problem and errmsg is set, log an error
func openConfigFile(path string, errmsg string) *clientcmdapi.Config {
	var err error
	var file *os.File
	if path == "" { // empty param/envvar
		return nil
	} else if file, err = os.Open(path); err == nil {
		log.Debugm("discOpenCC", log.Msg{"tmpl": "Reading client config at {{.path}}", "path": path})
	} else if errmsg == "" {
		log.Debugf("discOpenCCNo", "Could not read client config at %s:\n%#v", path, err)
	} else if os.IsNotExist(err) {
		log.Error("discOpenCCNoExist", errmsg+"but that file does not exist.")
	} else if os.IsPermission(err) {
		log.Error("discOpenCCNoPerm", errmsg+"but lack permission to read that file.")
	} else {
		log.Errorf("discOpenCCErr", "%sbut there was an error opening it:\n%#v", errmsg, err)
	}
	if file != nil { // it is open for reading
		defer file.Close()
		if buffer, err := ioutil.ReadAll(file); err != nil {
			log.Errorf("discCCReadErr", "Unexpected error while reading client config file (%s): %v", path, err)
		} else if conf, err := clientcmd.Load(buffer); err != nil {
			log.Errorf("discCCYamlErr", `
Error reading YAML from client config file (%s):
  %v
This file may have been truncated or mis-edited.
Please fix, remove, or obtain a new client config`, file.Name(), err)
		} else {
			log.Infom("discCCRead", log.Msg{"tmpl": `Successfully read a client config file at '{{.path}}'`, "path": path})
			/* Note, we're not going to use this config file directly.
			 * Instead, we'll defer to the openshift client code to assimilate
			 * flags, env vars, and the potential hierarchy of config files
			 * into an actual configuration that the client uses.
			 * However, for diagnostic purposes, record the first we find.
			 */
			return conf
		}
	}
	return nil
}

/* The full client configuration may specify multiple contexts, each
 * of which could be a different server, a different user, a different
 * default project. We want to check which contexts have useful access,
 * and record those. At this point, we should already have the factory
 * for the current context. Factories embed config and a client cache,
 * and since we want to do discovery for every available context, we are
 * going to create a factory for each context. We will determine which
 * context actually has access to the default project, preferring the
 * current (default) context if it does. Connection errors should be
 * diagnosed along the way.
 */
func configClient(env *types.Environment) {
	if env.OsConfig != nil {
		// TODO: run these in parallel, with a time limit so connection timeouts don't take forever
		for cname, context := range env.OsConfig.Contexts {
			// set context, create factory, see what's available
			if env.FactoryForContext[cname] == nil {
				//config := clientcmd.NewNonInteractiveClientConfig(env.Factory.OpenShiftClientConfig, cname, &clientcmd.ConfigOverrides{})
				config := clientcmd.NewNonInteractiveClientConfig(*env.OsConfig, cname, &clientcmd.ConfigOverrides{})
				f := osclientcmd.NewFactory(config)
				//f.BindFlags(env.Flags.OpenshiftFlags)
				env.FactoryForContext[cname] = f
			}
			if access := getContextAccess(env.FactoryForContext[cname], cname, context); access != nil {
				env.AccessForContext[cname] = access
				if access.ClusterAdmin && (cname == env.OsConfig.CurrentContext || env.ClusterAdminFactory == nil) {
					env.ClusterAdminFactory = env.FactoryForContext[cname]
				}
			}
		}
	}
}

// for now, only try to determine what namespaces a user can see
func getContextAccess(factory *osclientcmd.Factory, ctxName string, ctx clientcmdapi.Context) *types.ContextAccess {
	// start by getting ready to log the result
	msgText := "Testing client config context {{.context}}\nServer: {{.server}}\nUser: {{.user}}\n\n"
	msg := log.Msg{"id": "discCCctx", "tmpl": msgText}
	if config, err := factory.OpenShiftClientConfig.RawConfig(); err != nil {
		log.Errorf("discCCstart", "Could not read client config: (%T) %[1]v", err)
		return nil
	} else {
		msg["context"] = ctxName
		msg["server"] = config.Clusters[ctx.Cluster].Server
		msg["user"] = ctx.AuthInfo
	}
	// actually go and request project list from the server
	mapper, typer := factory.Object()
	obj, err := resource.NewBuilder(mapper, typer, factory.ClientMapperForCommand(nil)).
		ResourceTypeOrNameArgs(true, "projects").
		Latest().
		Do().
		Object()
	//Infos()
	if err == nil { // success!
		list := obj.(*pkgapi.ProjectList).Items
		if len(list) == 0 {
			msg["tmpl"] = msgText + "Successfully requested project list, but it is empty, so user has no access to anything."
			msg["projects"] = make([]string, 0)
			log.Infom("discCCctxSuccess", msg)
			return nil
		}
		access := &types.ContextAccess{Projects: make([]string, len(list))}
		for i, project := range list {
			access.Projects[i] = project.Name
			if project.Name == kapi.NamespaceDefault {
				access.ClusterAdmin = true
			}
		}
		if access.ClusterAdmin {
			msg["tmpl"] = msgText + "Successfully requested project list; has access to default project, so assumed to be a cluster-admin"
			log.Infom("discCCctxSuccess", msg)
		} else {
			msg["tmpl"] = msgText + "Successfully requested project list; has access to project(s): {{.projectStr}}"
			msg["projects"] = access.Projects
			msg["projectStr"] = strings.Join(access.Projects, ", ")
			log.Infom("discCCctxSuccess", msg)
		}
		return access
	} else { // something went wrong, so diagnose it
		noResolveRx := regexp.MustCompile("dial tcp: lookup (\\S+): no such host")
		unknownCaMsg := "x509: certificate signed by unknown authority"
		unneededCaMsg := "specifying a root certificates file with the insecure flag is not allowed"
		invalidCertNameRx := regexp.MustCompile("x509: certificate is valid for (\\S+, )+not (\\S+)")
		connRefusedRx := regexp.MustCompile("dial tcp (\\S+): connection refused")
		connTimeoutRx := regexp.MustCompile("dial tcp (\\S+): (?:connection timed out|i/o timeout)")
		unauthenticatedMsg := `403 Forbidden: Forbidden: "/osapi/v1beta1/projects?namespace=" denied by default`

		malformedHTTPMsg := "malformed HTTP response"
		malformedTLSMsg := "tls: oversized record received with length"

		// interpret the error message for mere mortals
		errm := err.Error()
		var reason, errId string
		switch {
		case noResolveRx.MatchString(errm):
			errId, reason = "clientNoResolve", `
This usually means that the hostname does not resolve to an IP.
Hostnames should usually be resolved via DNS or an /etc/hosts file.
Ensure that the hostname resolves correctly from your host before proceeding.
Of course, your config could also simply have the wrong hostname specified.
`
		case strings.Contains(errm, unknownCaMsg):
			errId, reason = "clientUnknownCa", `
This means that we cannot validate the certificate in use by the
OpenShift API server, so we cannot securely communicate with it.
Connections could be intercepted and your credentials stolen.

Since the server certificate we see when connecting is not validated
by public certificate authorities (CAs), you probably need to specify a
certificate from a private CA to validate the connection.

Your config may be specifying the wrong CA cert, or none, or there
could actually be a man-in-the-middle attempting to intercept your
connection.  If you are unconcerned about any of this, you can add the
--insecure-skip-tls-verify flag to bypass secure (TLS) verification,
but this is risky and should not be necessary.
** Connections could be intercepted and your credentials stolen. **
`
		case strings.Contains(errm, unneededCaMsg):
			errId, reason = "clientUnneededCa", `
This means that for client connections to the OpenShift API server, you
(or your kubeconfig) specified both a validating certificate authority
and that the client should bypass connection security validation.

This is not allowed because it is likely to be a mistake.

If you want to use --insecure-skip-tls-verify to bypass security (which
is usually a bad idea anyway), then you need to also clear the CA cert
from your command line options or kubeconfig file(s). Of course, it
would be far better to obtain and use a correct CA cert.
`
		case invalidCertNameRx.MatchString(errm):
			match := invalidCertNameRx.FindStringSubmatch(errm)
			serverHost := match[len(match)-1]
			errId, reason = "clientInvCertName", fmt.Sprintf(`
This means that the certificate in use by the OpenShift API server
(master) does not match the hostname by which you are addressing it:
  %s
so a secure connection is not allowed. In theory, this *could* mean that
someone is intercepting your connection and presenting a certificate
that is valid but for a different server, which is why secure validation
fails in this case.

However, the most likely explanation is that the server certificate
needs to be updated to include the name you are using to reach it.

If the OpenShift server is generating its own certificates (which
is default), then the --public-master flag on the OpenShift master is
usually the easiest way to do this. If you need something more complicated
(for instance, multiple public addresses for the API, or your own CA),
then you will need to custom-generate the server certificate with the
right names yourself.

If you are unconcerned about any of this, you can add the
--insecure-skip-tls-verify flag to bypass secure (TLS) verification,
but this is risky and should not be necessary.
** Connections could be intercepted and your credentials stolen. **
`, serverHost)
		case connRefusedRx.MatchString(errm):
			errId, reason = "clientInvCertName", `
This means that when we tried to connect to the OpenShift API
server (master), we reached the host, but nothing accepted the port
connection. This could mean that the OpenShift master is stopped, or
that a firewall or security policy is blocking access at that port.

You will not be able to connect or do anything at all with OpenShift
until this server problem is resolved or you specify a corrected
server address.`
		case connTimeoutRx.MatchString(errm):
			errId, reason = "clientConnTimeout", `
This means that when we tried to connect to the OpenShift API server
(master), we could not reach the host at all.
* You may have specified the wrong host address.
* This could mean the host is completely unavailable (down).
* This could indicate a routing problem or a firewall that simply
  drops requests rather than responding by reseting the connection.
* It does not generally mean that DNS name resolution failed (which
  would be a different error) though the problem could be that it
  gave the wrong address.`
		case strings.Contains(errm, malformedHTTPMsg):
			errId, reason = "clientMalformedHTTP", `
This means that when we tried to connect to the OpenShift API server
(master) with a plain HTTP connection, the server did not speak
HTTP back to us. The most common explanation is that a secure server
is listening but you specified an http: connection instead of https:.
There could also be another service listening at the intended port
speaking some other protocol entirely.

You will not be able to connect or do anything at all with OpenShift
until this server problem is resolved or you specify a corrected
server address.`
		case strings.Contains(errm, malformedTLSMsg):
			errId, reason = "clientMalformedTLS", `
This means that when we tried to connect to the OpenShift API server
(master) with a secure HTTPS connection, the server did not speak
HTTPS back to us. The most common explanation is that the server
listening at that port is not the secure server you expected - it
may be a non-secure HTTP server or the wrong service may be
listening there, or you may have specified an incorrect port.

You will not be able to connect or do anything at all with OpenShift
until this server problem is resolved or you specify a corrected
server address.`
		case strings.Contains(errm, unauthenticatedMsg):
			errId, reason = "clientUnauth", `
This means that when we tried to make a request to the OpenShift API
server, your kubeconfig did not present valid credentials to
authenticate your client. Credentials generally consist of a client
key/certificate or an access token. Your kubeconfig may not have
presented any, or they may be invalid.`
		default:
			errId, reason = "clientUnknownConnErr", `Diagnostics does not have an explanation for what this means. Please report this error so one can be added.`
		}
		errMsg := fmt.Sprintf("(%T) %[1]v", err)
		msg["tmpl"] = msgText + errMsg + reason
		msg["errMsg"] = errMsg
		log.Errorm(errId, msg)
	}
	return nil
}