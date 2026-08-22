(.configDigest == $configDigest) and
(.controlApi == {sourceDigest:$sourceDigest,image:$image,imageId:$imageId}) and
(.secretDigests == {"workspace-invitation-hmac-v1":$secretDigest}) and
(.nodePlanReceiptDigest == $nodePlanReceiptDigest) and
(if $issuerMaterialDigest == "" then
   .schemaVersion == "blazn.dev/control-plane-backup/v3" and
   (has("microk8sIssuerMaterialDigest") | not)
 else
   .schemaVersion == "blazn.dev/control-plane-backup/v4" and
   .microk8sIssuerMaterialDigest == $issuerMaterialDigest
 end)
