package pxc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	api "github.com/percona/percona-xtradb-cluster-operator/pkg/apis/pxc/v1"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc/users"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const internalPrefix = "internal-"

func (r *ReconcilePerconaXtraDBCluster) reconcileUsers(cr *api.PerconaXtraDBCluster) (map[string]string, map[string]string, error) {
	sysUsersSecretObj := corev1.Secret{}
	err := r.client.Get(context.TODO(),
		types.NamespacedName{
			Namespace: cr.Namespace,
			Name:      cr.Spec.SecretsName,
		},
		&sysUsersSecretObj,
	)
	if err != nil && k8serrors.IsNotFound(err) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, errors.Wrapf(err, "get sys users secret '%s'", cr.Spec.SecretsName)
	}

	secretName := internalPrefix + cr.Name

	internalSysSecretObj := corev1.Secret{}

	err = r.client.Get(context.TODO(),
		types.NamespacedName{
			Namespace: cr.Namespace,
			Name:      secretName,
		},
		&internalSysSecretObj,
	)
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, nil, errors.Wrap(err, "get internal sys users secret")
	}

	if k8serrors.IsNotFound(err) {
		internalSysUsersSecret := sysUsersSecretObj.DeepCopy()
		internalSysUsersSecret.ObjectMeta = metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cr.Namespace,
		}
		err = r.client.Create(context.TODO(), internalSysUsersSecret)
		if err != nil {
			return nil, nil, errors.Wrap(err, "create internal sys users secret")
		}
		return nil, nil, nil
	}

	if cr.Status.PXC.Ready > 0 {
		err := r.manageOperatorAdminUser(cr, &sysUsersSecretObj, &internalSysSecretObj)
		if err != nil {
			return nil, nil, errors.Wrap(err, "manage operator admin user")
		}
	}

	if cr.Status.Status != api.AppStateReady {
		return nil, nil, nil
	}

	newSysData, err := json.Marshal(sysUsersSecretObj.Data)
	if err != nil {
		return nil, nil, errors.Wrap(err, "marshal sys secret data")
	}
	newSecretDataHash := sha256Hash(newSysData)

	dataChanged, err := sysUsersSecretDataChanged(newSecretDataHash, &internalSysSecretObj)
	if err != nil {
		return nil, nil, errors.Wrap(err, "check sys users data changes")
	}

	if !dataChanged {
		return nil, nil, nil
	}

	restartPXC, restartProxy, err := r.manageSysUsers(cr, &sysUsersSecretObj, &internalSysSecretObj)
	if err != nil {
		return nil, nil, errors.Wrap(err, "manage sys users")
	}

	internalSysSecretObj.Data = sysUsersSecretObj.Data
	err = r.client.Update(context.TODO(), &internalSysSecretObj)
	if err != nil {
		return nil, nil, errors.Wrap(err, "update internal sys users secret")
	}
	pxcAnnotations := make(map[string]string)
	proxysqlAnnotations := make(map[string]string)

	if restartProxy {
		proxysqlAnnotations["last-applied-secret"] = newSecretDataHash
	}
	if restartPXC {
		pxcAnnotations["last-applied-secret"] = newSecretDataHash
	}

	return pxcAnnotations, proxysqlAnnotations, nil
}

func (r *ReconcilePerconaXtraDBCluster) manageSysUsers(cr *api.PerconaXtraDBCluster, sysUsersSecretObj, internalSysSecretObj *corev1.Secret) (bool, bool, error) {
	var restartPXC, restartProxy, syncProxySQLUsers bool

	var sysUsers []users.SysUser
	var proxyUsers []users.SysUser

	type user struct {
		name  string
		hosts []string
	}
	requiredUsers := []user{
		{
			name:  "root",
			hosts: []string{"localhost", "%"},
		},
		{
			name:  "xtrabackup",
			hosts: []string{"localhost"},
		},
		{
			name:  "monitor",
			hosts: []string{"%"},
		},
		{
			name:  "clustercheck",
			hosts: []string{"localhost"},
		},
		{
			name:  "operator",
			hosts: []string{"%"},
		},
	}
	if cr.Spec.PMM.Enabled {
		requiredUsers = append(requiredUsers, user{
			name: "pmmserver",
		})
	}
	if cr.Spec.ProxySQL.Enabled {
		requiredUsers = append(requiredUsers, user{
			name: "proxyadmin",
		})
	}

	for _, user := range requiredUsers {
		if len(sysUsersSecretObj.Data[user.name]) == 0 {
			return false, false, errors.New("undefined or not exist user " + user.name)
		}

		if bytes.Compare(sysUsersSecretObj.Data[user.name], internalSysSecretObj.Data[user.name]) == 0 {
			continue
		}

		pass := string(sysUsersSecretObj.Data[user.name])

		switch user.name {
		case "root":
			syncProxySQLUsers = true
		case "xtrabackup":
			restartPXC = true
		case "monitor":
			restartProxy = true
			proxyUsers = append(proxyUsers, users.SysUser{Name: user.name, Pass: pass})
			if cr.Spec.PMM.Enabled {
				restartPXC = true
			}
		case "clustercheck":
			restartPXC = true
		case "proxyadmin":
			restartProxy = true
			proxyUsers = append(proxyUsers, users.SysUser{Name: user.name, Pass: pass})
			continue
		case "pmmserver":
			if cr.Spec.PMM.Enabled {
				restartProxy = true
				restartPXC = true
				continue
			}
		case "operator":
			restartProxy = true
		}
		user := users.SysUser{
			Name:  user.name,
			Pass:  pass,
			Hosts: user.hosts,
		}
		sysUsers = append(sysUsers, user)
	}

	pxcUser := "root"
	pxcPass := string(internalSysSecretObj.Data["root"])
	if _, ok := sysUsersSecretObj.Data["operator"]; ok {
		pxcUser = "operator"
		pxcPass = string(internalSysSecretObj.Data["operator"])
	}

	um, err := users.NewManager(cr.Name+"-pxc."+cr.Namespace, pxcUser, pxcPass)
	if err != nil {
		return restartPXC, restartProxy, errors.Wrap(err, "new users manager")
	}
	defer um.Close()

	if len(sysUsers) > 0 {
		err = um.UpdateUsersPass(sysUsers)
		if err != nil {
			return restartPXC, restartProxy, errors.Wrap(err, "update sys users pass")
		}
	}

	if len(proxyUsers) > 0 {
		err = updateProxyUsers(proxyUsers, internalSysSecretObj, cr)
		if err != nil {
			return restartPXC, restartProxy, errors.Wrap(err, "update Proxy users pass")
		}
	}

	if syncProxySQLUsers && !restartProxy {
		err = r.syncPXCUsersWithProxySQL(cr)
		if err != nil {
			return restartPXC, restartProxy, errors.Wrap(err, "sync users")
		}
	}

	return restartPXC, restartProxy, nil
}

func (r *ReconcilePerconaXtraDBCluster) syncPXCUsersWithProxySQL(cr *api.PerconaXtraDBCluster) error {
	if cr.Status.Status != api.AppStateReady || cr.Status.ProxySQL.Status != api.AppStateReady {
		return nil
	}
	// sync users if ProxySql enabled
	if cr.Spec.ProxySQL != nil && cr.Spec.ProxySQL.Enabled {
		pod := corev1.Pod{}
		err := r.client.Get(context.TODO(),
			types.NamespacedName{
				Namespace: cr.Namespace,
				Name:      cr.Name + "-proxysql-0",
			},
			&pod,
		)
		if err != nil {
			return errors.Wrap(err, "get proxysql pod")
		}
		var errb, outb bytes.Buffer
		err = r.clientcmd.Exec(&pod, "proxysql", []string{"proxysql-admin", "--syncusers"}, nil, &outb, &errb, false)
		if err != nil {
			return errors.Errorf("exec syncusers: %v / %s / %s", err, outb.String(), errb.String())
		}
		if len(errb.Bytes()) > 0 {
			return errors.New("syncusers: " + errb.String())
		}
	}

	return nil
}

func updateProxyUsers(proxyUsers []users.SysUser, internalSysSecretObj *corev1.Secret, cr *api.PerconaXtraDBCluster) error {
	um, err := users.NewManager(cr.Name+"-proxysql-unready."+cr.Namespace+":6032", "proxyadmin", string(internalSysSecretObj.Data["proxyadmin"]))
	if err != nil {
		return errors.Wrap(err, "new users manager")
	}
	defer um.Close()

	err = um.UpdateProxyUsers(proxyUsers)
	if err != nil {
		return errors.Wrap(err, "update proxy users")
	}

	return nil
}

func (r *ReconcilePerconaXtraDBCluster) manageOperatorAdminUser(cr *api.PerconaXtraDBCluster, sysUsersSecretObj, internalSysSecretObj *corev1.Secret) error {
	pass, existInSys := sysUsersSecretObj.Data["operator"]
	_, existInInternal := internalSysSecretObj.Data["operator"]
	if existInSys && !existInInternal {
		if internalSysSecretObj.Data == nil {
			internalSysSecretObj.Data = make(map[string][]byte)
		}
		internalSysSecretObj.Data["operator"] = pass
		return nil
	}
	if existInSys {
		return nil
	}

	pass, err := generatePass()
	if err != nil {
		return errors.Wrap(err, "generate password")
	}

	um, err := users.NewManager(cr.Name+"-pxc."+cr.Namespace, "root", string(sysUsersSecretObj.Data["root"]))
	if err != nil {
		return errors.Wrap(err, "new users manager")
	}
	defer um.Close()

	err = um.CreateOperatorUser(string(pass))
	if err != nil {
		return errors.Wrap(err, "create operator user")
	}

	sysUsersSecretObj.Data["operator"] = pass
	internalSysSecretObj.Data["operator"] = pass

	err = r.client.Update(context.TODO(), sysUsersSecretObj)
	if err != nil {
		return errors.Wrap(err, "update sys users secret")
	}
	err = r.client.Update(context.TODO(), internalSysSecretObj)
	if err != nil {
		return errors.Wrap(err, "update internal users secret")
	}

	return nil
}

func sysUsersSecretDataChanged(newHash string, usersSecret *corev1.Secret) (bool, error) {
	secretData, err := json.Marshal(usersSecret.Data)
	if err != nil {
		return true, err
	}
	oldHash := sha256Hash(secretData)

	if oldHash != newHash {
		return true, nil
	}

	return false, nil
}

func sha256Hash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
