output "kubeconfigs" {
  value = [for cl in module.eks : pathexpand(format("~/.kube/%s", cl.cluster_id))]
}

output "efs_id" {
  value = aws_efs_file_system.this[*].id
}
