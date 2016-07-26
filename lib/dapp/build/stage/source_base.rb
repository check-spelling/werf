module Dapp
  module Build
    module Stage
      # Base of source stages
      class SourceBase < Base
        attr_accessor :prev_source_stage, :next_source_stage

        GITARTIFACT_IMAGE = 'dappdeps/gitartifact:0.1.3'.freeze

        def prev_source_stage
          prev_stage.prev_stage
        end

        def next_source_stage
          next_stage.next_stage
        end

        def save_in_cache!
          super
          layers_commits_write!
        end

        def signature
          hashsum [dependencies_checksum, *commit_list]
        end

        def image
          super do |image|
            application.git_artifacts.each do |git_artifact|
              image.add_volume "#{git_artifact.repo.dir_path}:#{git_artifact.repo.container_build_dir_path}"
              image.add_commands git_artifact.send(apply_command_method, self)
            end

            if should_be_with_git?
              image.add_volumes_from(gitartifact_container)
              image.unshift_commands 'export PATH=/.dapp/deps/gitartifact/bin:$PATH'
            end
            yield image if block_given?
          end
        end

        def dependencies_checksum
          hashsum [prev_stage.signature]
        end

        def layer_commit(git_artifact)
          commits[git_artifact] ||= begin
            if layer_commit_file_path(git_artifact).exist?
              layer_commit_file_path(git_artifact).read.strip
            else
              git_artifact.latest_commit
            end
          end
        end

        protected

        def gitartifact_container_name # FIXME: hashsum(image) or dockersafe()
          GITARTIFACT_IMAGE.tr('/', '_').tr(':', '_')
        end

        def gitartifact_container
          @gitartifact_container ||= begin
            if application.shellout("docker inspect #{gitartifact_container_name}").exitstatus != 0
              application.log_secondary_process(application.t(code: 'process.git_artifact_loading'), short: true) do
                application.shellout ['docker run',
                                      '--restart=no',
                                      "--name #{gitartifact_container_name}",
                                      "--volume /.dapp/deps/gitartifact #{GITARTIFACT_IMAGE}",
                                      '2>/dev/null'].join(' ')
              end
            end
            gitartifact_container_name
          end
        end

        def should_be_not_detailed?
          true
        end

        def should_be_with_git?
          application.git_artifacts.any? && !image.send(:bash_commands).empty?
        end

        def apply_command_method
          :apply_patch_command
        end

        def commit_list
          application.git_artifacts.map { |git_artifact| layer_commit(git_artifact) }
        end

        def layers_commits_write!
          application.git_artifacts.each { |git_artifact| layer_commit_file_path(git_artifact).write(layer_commit(git_artifact)) }
        end

        def layer_commit_file_path(git_artifact)
          application.build_path git_artifact.filename ".#{name}.#{git_artifact.paramshash}.#{dependencies_checksum}.commit"
        end

        def dependency_files_checksum(regs)
          hashsum(regs.map { |reg| Dir[File.join(application.home_path, reg)].map { |f| File.read(f) if File.file?(f) } })
        end

        private

        def commits
          @commits ||= {}
        end
      end # SourceBase
    end # Stage
  end # Build
end # Dapp
